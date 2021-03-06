package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/openshift/csi-driver-manila-operator/pkg/controllers/manila"
	"github.com/openshift/csi-driver-manila-operator/pkg/controllers/secret"
	"github.com/openshift/csi-driver-manila-operator/pkg/generated"
	"github.com/openshift/csi-driver-manila-operator/pkg/util"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	csicontrollerset "github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	csidrivercontroller "github.com/openshift/library-go/pkg/operator/csi/csidrivercontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	operandName  = "manila-csi-driver"
	operatorName = "manila-csi-driver-operator"

	nfsImageEnvName = "NFS_DRIVER_IMAGE"

	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, util.OperandNamespace, "")

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	// Create GenericOperatorclient. This is used by controllers created down below
	gvr := operatorapi.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, string(operatorapi.ManilaCSIDriver))
	if err != nil {
		return err
	}

	csiDriverControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		operandName,
		false,
	).WithStaticResourcesController(
		"ManilaDriverStaticResources",
		kubeClient,
		kubeInformersForNamespaces,
		generated.Asset,
		[]string{
			"namespace.yaml",
			"csidriver.yaml",
			"controller_sa.yaml",
			"node_sa.yaml",
			"rbac/snapshotter_binding.yaml",
			"rbac/snapshotter_role.yaml",
			"rbac/provisioner_binding.yaml",
			"rbac/provisioner_role.yaml",
			"rbac/privileged_role.yaml",
			"rbac/controller_privileged_binding.yaml",
			"rbac/node_privileged_binding.yaml",
		},
	).WithCSIDriverController(
		"ManilaDriverController",
		factory.DefaultQueueKey,
		operandName,
		util.OperandNamespace,
		assetWithNFSDriver,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(util.OperandNamespace),
		csicontrollerset.WithControllerService("controller.yaml"),
		csicontrollerset.WithNodeService("node.yaml"),
	).WithCredentialsRequestController(
		"ManilaDriverCredentials",
		util.OperandNamespace,
		generated.MustAsset,
		"credentials.yaml",
		dynamicClient)

	nfsCSIDriverController := csidrivercontroller.NewCSIDriverController(
		"NFSDriverController",
		"nfs-csi-driver",
		util.OperandNamespace,
		string(operatorapi.ManilaCSIDriver),
		operatorClient,
		assetWithNFSDriver,
		kubeClient,
		controllerConfig.EventRecorder,
	).WithNodeService(kubeInformersForNamespaces.InformersFor(util.OperandNamespace).Apps().V1().DaemonSets(), "node_nfs.yaml")

	// sync config map with OpenStack CA certificate to the operand namespace,
	// so the driver can get it as a ConfigMap volume.
	srcConfigMap := resourcesynccontroller.ResourceLocation{
		Namespace: util.CloudConfigNamespace,
		Name:      util.CloudConfigName,
	}
	dstConfigMap := resourcesynccontroller.ResourceLocation{
		Namespace: util.OperandNamespace,
		Name:      util.CloudConfigName,
	}
	certController := resourcesynccontroller.NewResourceSyncController(
		operatorClient,
		kubeInformersForNamespaces,
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		controllerConfig.EventRecorder)
	certController.SyncConfigMap(dstConfigMap, srcConfigMap)

	openstackClient, err := manila.NewOpenStackClient(util.CloudConfigFilename, kubeInformersForNamespaces)
	if err != nil {
		return err
	}

	secretSyncController := secret.NewSecretSyncController(
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		resync,
		controllerConfig.EventRecorder)

	manilaController := manila.NewManilaController(
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		openstackClient,
		[]manila.Runnable{
			csiDriverControllerSet,
			nfsCSIDriverController,
			secretSyncController,
			certController,
		},
		controllerConfig.EventRecorder,
	)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())

	klog.Info("Starting controllers")
	go manilaController.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

// CSIDriverController can replace only a single driver in driver manifests.
// Manila needs to replace two of them: Manila driver and NFS driver image.
// Let the Manila image be replaced by CSIDriverController and NFS in this
// custom asset loading func.
func assetWithNFSDriver(file string) []byte {
	asset := generated.MustAsset(file)
	nfsImage := os.Getenv(nfsImageEnvName)
	if nfsImage == "" {
		return asset
	}
	return bytes.ReplaceAll(asset, []byte("${NFS_DRIVER_IMAGE}"), []byte(nfsImage))
}
