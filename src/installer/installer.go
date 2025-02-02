package installer

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/go-openapi/swag"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"

	"github.com/openshift/assisted-installer/src/common"
	"github.com/openshift/assisted-installer/src/config"
	"github.com/openshift/assisted-installer/src/ignition"
	"github.com/openshift/assisted-installer/src/inventory_client"
	"github.com/openshift/assisted-installer/src/k8s_client"
	"github.com/openshift/assisted-installer/src/ops"
	"github.com/openshift/assisted-installer/src/utils"
	"github.com/openshift/assisted-service/models"
)

const (
	InstallDir                   = "/opt/install-dir"
	KubeconfigPath               = "/opt/openshift/auth/kubeconfig"
	minMasterNodes               = 2
	dockerConfigFile             = "/root/.docker/config.json"
	assistedControllerNamespace  = "assisted-installer"
	extractRetryCount            = 3
	waitForeverTimeout           = time.Duration(1<<63 - 1) // wait forever ~ 292 years
	ovnKubernetes                = "OVNKubernetes"
	numMasterNodes               = 3
	singleNodeMasterIgnitionPath = "/opt/openshift/master.ign"
)

var generalWaitTimeout = 30 * time.Second
var generalWaitInterval = 5 * time.Second

// Installer will run the install operations on the node
type Installer interface {
	InstallNode() error
	UpdateHostInstallProgress(newStage models.HostStage, info string)
}

type installer struct {
	config.Config
	log             *logrus.Logger
	ops             ops.Ops
	inventoryClient inventory_client.InventoryClient
	kcBuilder       k8s_client.K8SClientBuilder
	ign             ignition.Ignition
}

func NewAssistedInstaller(log *logrus.Logger, cfg config.Config, ops ops.Ops, ic inventory_client.InventoryClient, kcb k8s_client.K8SClientBuilder, ign ignition.Ignition) *installer {
	return &installer{
		log:             log,
		Config:          cfg,
		ops:             ops,
		inventoryClient: ic,
		kcBuilder:       kcb,
		ign:             ign,
	}
}

func (i *installer) InstallNode() error {
	i.log.Infof("Installing node with role: %s", i.Config.Role)

	i.UpdateHostInstallProgress(models.HostStageStartingInstallation, i.Config.Role)
	i.Config.Device = i.ops.EvaluateDiskSymlink(i.Config.Device)
	i.log.Infof("Start cleaning up device %s", i.Device)
	err := i.cleanupInstallDevice()
	if err != nil {
		i.log.Errorf("failed to prepare install device %s, err %s", i.Device, err)
		return err
	}
	i.log.Infof("Finished cleaning up device %s", i.Device)

	if err = i.ops.Mkdir(InstallDir); err != nil {
		i.log.Errorf("Failed to create install dir: %s", err)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	bootstrapErrGroup, _ := errgroup.WithContext(ctx)
	//cancel the context in case this method ends
	defer cancel()
	isBootstrap := false
	if i.Config.Role == string(models.HostRoleBootstrap) && i.HighAvailabilityMode != models.ClusterHighAvailabilityModeNone {
		isBootstrap = true
		bootstrapErrGroup.Go(func() error {
			return i.startBootstrap()
		})
		go i.updateConfiguringStatus(ctx)
		i.Config.Role = string(models.HostRoleMaster)
	}

	i.UpdateHostInstallProgress(models.HostStageInstalling, i.Config.Role)
	var ignitionPath string
	if i.HighAvailabilityMode == models.ClusterHighAvailabilityModeNone {
		i.log.Info("Installing single node openshift")
		ignitionPath, err = i.createSingleNodeMasterIgnition()
		if err != nil {
			return err
		}
	} else {
		ignitionPath, err = i.downloadHostIgnition()
		if err != nil {
			return err
		}

	}

	if err = i.writeImageToDisk(ignitionPath); err != nil {
		return err
	}

	if err = i.ops.SetBootOrder(i.Device); err != nil {
		i.log.WithError(err).Warnf("Failed to set boot order")
		// Ignore the error for now so it doesn't fail the installation in case it fails
		//return err
	}

	if isBootstrap {
		if err = bootstrapErrGroup.Wait(); err != nil {
			i.log.Errorf("Bootstrap failed %s", err)
			return err
		}
		if err = i.waitForControlPlane(ctx); err != nil {
			return err
		}
		i.log.Info("Setting bootstrap node new role to master")

	} else if i.Config.Role == string(models.HostRoleWorker) {
		// Wait for 2 masters to be ready before rebooting
		if err = i.workerWaitFor2ReadyMasters(ctx); err != nil {
			return err
		}
	}
	//upload host logs and report log status before reboot
	i.log.Infof("Uploading logs and reporting status before rebooting the node %s for cluster %s", i.Config.HostID, i.Config.ClusterID)
	i.inventoryClient.HostLogProgressReport(ctx, i.Config.InfraEnvID, i.Config.HostID, models.LogsStateRequested)
	_, err = i.ops.UploadInstallationLogs(isBootstrap || i.HighAvailabilityMode == models.ClusterHighAvailabilityModeNone)
	if err != nil {
		i.log.Errorf("upload installation logs %s", err)
	}
	//update installation progress
	i.UpdateHostInstallProgress(models.HostStageRebooting, "")
	//reboot
	if err = i.ops.Reboot(); err != nil {
		return err
	}
	return nil
}

//updateSingleNodeIgnition will download the host ignition config and add the files under storage
func (i *installer) updateSingleNodeIgnition(singleNodeIgnitionPath string) error {
	hostIgnitionPath, err := i.downloadHostIgnition()
	if err != nil {
		return err
	}
	fmt.Println(i.ign)
	singleNodeconfig, err := i.ign.ParseIgnitionFile(singleNodeIgnitionPath)
	if err != nil {
		return err
	}
	hostConfig, err := i.ign.ParseIgnitionFile(hostIgnitionPath)
	if err != nil {
		return err
	}
	// TODO: update this once we can get the full host specific overrides we have in the ignition
	// Remove the Config part since we only want the rest of the overrides
	hostConfig.Ignition.Config = ignition.EmptyIgnitionConfig
	merged, mergeErr := i.ign.MergeIgnitionConfig(singleNodeconfig, hostConfig)
	if mergeErr != nil {
		return errors.Wrapf(mergeErr, "failed to apply host ignition config overrides")
	}
	err = i.ign.WriteIgnitionFile(singleNodeIgnitionPath, merged)
	if err != nil {
		return err
	}
	return nil
}

func (i *installer) writeImageToDisk(ignitionPath string) error {
	i.UpdateHostInstallProgress(models.HostStageWritingImageToDisk, "")
	interval := time.Second
	err := utils.Retry(3, interval, i.log, func() error {
		return i.ops.WriteImageToDisk(ignitionPath, i.Device, i.inventoryClient, i.Config.InstallerArgs)
	})
	if err != nil {
		i.log.Errorf("Failed to write image to disk %s", err)
		return err
	}
	i.log.Info("Done writing image to disk")
	return nil
}

func (i *installer) startBootstrap() error {
	i.log.Infof("Running bootstrap")
	// This is required for the log collection command to work since it will try to mount this directory
	// This directory is also required by `generateSshKeyPair` as it will place the key there
	if err := i.ops.Mkdir(sshDir); err != nil {
		i.log.WithError(err).Error("Failed to create SSH dir")
		return err
	}
	ignitionFileName := "bootstrap.ign"
	ignitionPath, err := i.getFileFromService(ignitionFileName)
	if err != nil {
		return err
	}

	// We need to extract pull secret from ignition and save it in docker config
	// to be able to pull MCO official image
	if err = i.ops.ExtractFromIgnition(ignitionPath, dockerConfigFile); err != nil {
		return err
	}

	err = i.extractIgnitionToFS(ignitionPath)
	if err != nil {
		return err
	}

	if i.HighAvailabilityMode != models.ClusterHighAvailabilityModeNone {
		err = i.generateSshKeyPair()
		if err != nil {
			return err
		}
		err = i.ops.CreateOpenshiftSshManifest(assistedInstallerSshManifest, sshManifestTmpl, sshPubKeyPath)
		if err != nil {
			return err
		}
	}

	// reload systemd configurations from filesystem and regenerate dependency trees
	err = i.ops.SystemctlAction("daemon-reload")
	if err != nil {
		return err
	}

	/* in case hostname is localhost, we need to set hostname to some random value, in order to
	   disable network manager activating hostname service, which will in turn may reset /etc/resolv.conf and
	   remove the work done by 30-local-dns-prepender. This will cause DNS issue in bootkube and it will fail to complete
	   successfully
	*/
	err = i.checkLocalhostName()
	if err != nil {
		i.log.Error(err)
		return err
	}

	// restart NetworkManager to trigger NetworkManager/dispatcher.d/30-local-dns-prepender
	err = i.ops.SystemctlAction("restart", "NetworkManager.service")
	if err != nil {
		i.log.Error(err)
		return err
	}

	if err = i.ops.PrepareController(); err != nil {
		i.log.Error(err)
		return err
	}

	servicesToStart := []string{"bootkube.service", "approve-csr.service", "progress.service"}
	for _, service := range servicesToStart {
		err = i.ops.SystemctlAction("start", service)
		if err != nil {
			return err
		}
	}
	i.log.Info("Done setting up bootstrap")
	return nil
}

func (i *installer) extractIgnitionToFS(ignitionPath string) (err error) {
	mcoImage := i.MCOImage

	i.log.Infof("Extracting ignition to disk using %s mcoImage", mcoImage)
	for j := 0; j < extractRetryCount; j++ {
		_, err = i.ops.ExecPrivilegeCommand(utils.NewLogWriter(i.log), "podman", "run", "--net", "host",
			"--pid=host",
			"--volume", "/:/rootfs:rw",
			"--volume", "/usr/bin/rpm-ostree:/usr/bin/rpm-ostree",
			"--privileged",
			"--entrypoint", "/usr/bin/machine-config-daemon",
			mcoImage,
			"start", "--node-name", "localhost", "--root-mount", "/rootfs", "--once-from", ignitionPath, "--skip-reboot")
		if err != nil {
			i.log.WithError(err).Error("Failed to extract ignition to disk")
		} else {
			i.log.Info("Done extracting ignition to filesystem")
			return nil
		}
	}
	i.log.Errorf("Failed to extract ignition to disk, giving up")
	return err
}

func (i *installer) generateSshKeyPair() error {
	i.log.Info("Generating new SSH key pair")
	if _, err := i.ops.ExecPrivilegeCommand(utils.NewLogWriter(i.log), "ssh-keygen", "-q", "-f", sshKeyPath, "-N", ""); err != nil {
		i.log.WithError(err).Error("Failed to generate SSH key pair")
		return err
	}
	return nil
}

func (i *installer) getFileFromService(filename string) (string, error) {
	ctx := utils.GenerateRequestContext()
	log := utils.RequestIDLogger(ctx, i.log)
	log.Infof("Getting %s file", filename)
	dest := filepath.Join(InstallDir, filename)
	err := i.inventoryClient.DownloadFile(ctx, filename, dest)
	if err != nil {
		log.Errorf("Failed to fetch file (%s) from server. err: %s", filename, err)
	}
	return dest, err
}

func (i *installer) downloadHostIgnition() (string, error) {
	ctx := utils.GenerateRequestContext()
	log := utils.RequestIDLogger(ctx, i.log)
	filename := fmt.Sprintf("%s-%s.ign", i.Config.Role, i.Config.HostID)
	log.Infof("Getting %s file", filename)

	dest := filepath.Join(InstallDir, filename)
	err := i.inventoryClient.DownloadHostIgnition(ctx, i.Config.InfraEnvID, i.Config.HostID, dest)
	if err != nil {
		log.Errorf("Failed to fetch file (%s) from server. err: %s", filename, err)
	}
	return dest, err
}

func (i *installer) waitForNetworkType(kc k8s_client.K8SClient) error {
	return utils.WaitForPredicate(waitForeverTimeout, 5*time.Second, func() bool {
		_, err := kc.GetNetworkType()
		if err != nil {
			i.log.WithError(err).Error("Failed to get network type")
		}
		return err == nil
	})
}

func (i *installer) waitForControlPlane(ctx context.Context) error {
	err := i.ops.ReloadHostFile("/etc/resolv.conf")
	if err != nil {
		i.log.WithError(err).Error("Failed to reload resolv.conf")
		return err
	}
	kc, err := i.kcBuilder(KubeconfigPath, i.log)
	if err != nil {
		i.log.Error(err)
		return err
	}
	i.UpdateHostInstallProgress(models.HostStageWaitingForControlPlane, "")

	if err = i.waitForMinMasterNodes(ctx, kc); err != nil {
		return err
	}

	patch, err := utils.EtcdPatchRequired(i.Config.OpenshiftVersion)
	if err != nil {
		i.log.Error(err)
		return err
	}
	if patch {
		if err := kc.PatchEtcd(); err != nil {
			i.log.Error(err)
			return err
		}
	} else {
		i.log.Infof("Skipping etcd patch for cluster version %s", i.Config.OpenshiftVersion)
	}

	i.waitForBootkube(ctx)

	// waiting for controller pod to be running
	if err := i.waitForController(kc); err != nil {
		i.log.Error(err)
		return err
	}

	return nil
}

func numDoneMasters(cluster *models.Cluster) int {
	numDoneMasters := 0
	for _, h := range cluster.Hosts {
		if h.Role == models.HostRoleMaster && h.Progress.CurrentStage == models.HostStageDone {
			numDoneMasters++
		}
	}
	return numDoneMasters
}

func (i *installer) workerWaitFor2ReadyMasters(ctx context.Context) error {
	i.log.Info("Waiting for 2 ready masters")
	i.UpdateHostInstallProgress(models.HostStageWaitingForControlPlane, "")
	for {
		cluster, err := i.inventoryClient.GetCluster(ctx)
		if err != nil {
			i.log.WithError(err).Errorf("Getting cluster %s", i.ClusterID)
			return err
		}
		if swag.StringValue(cluster.Kind) == models.ClusterKindAddHostsCluster || numDoneMasters(cluster) >= minMasterNodes {
			return nil
		}
		time.Sleep(generalWaitInterval)
	}
}

func (i *installer) shouldControlPlaneReplicasPatchApplied(kc k8s_client.K8SClient) (bool, error) {
	controlPlanePatchRequired, err := utils.IsVersionLessThan47(i.Config.OpenshiftVersion)
	if err != nil {
		i.log.WithError(err).Errorf("Failed to get compare OCP version")
		return false, err
	}
	if !controlPlanePatchRequired {
		i.log.Info("Control plane replicas patch not required due to Openshift version not less than 4.7")
		return false, nil
	}
	if err = i.waitForNetworkType(kc); err != nil {
		i.log.WithError(err).Error("failed to wait for network type")
		return false, err
	}
	nt, err := kc.GetNetworkType()
	if err != nil {
		i.log.WithError(err).Error("failed to get network type")
		return false, err
	}
	if nt != ovnKubernetes {
		i.log.Info("Control plane replicas patch not required due to network type not OVNKubernetes")
		return false, nil
	}
	// OVNKubernetes waits the number that is defined in controlPlane.Replicas to be
	// available before starting OVN.  Since assisted installer has a bootstrap node
	// that later becomes a master, there is a need to patch the install-config to
	// set the controlPlane.replicas to 2 until all masters which are not the
	// bootstrap are ready.
	// On single node this is not the case since bootstrap in place is used.
	// Therefore, the patch is not relevant to single node.
	origControlPlaneReplicas, err := kc.GetControlPlaneReplicas()
	if err != nil {
		i.log.WithError(err).Error("Failed to get control plane replicas")
		return false, err
	}
	if origControlPlaneReplicas != numMasterNodes {
		i.log.Infof("Control plane replicas patch not required due to control plane replicas %d not equal to %d", origControlPlaneReplicas, numMasterNodes)
		return false, nil
	}
	i.log.Info("Applying control plane replicas patch")
	return true, nil
}

func (i *installer) waitForMinMasterNodes(ctx context.Context, kc k8s_client.K8SClient) error {
	shouldPatchControlPlaneReplicas, err := i.shouldControlPlaneReplicasPatchApplied(kc)
	if err != nil {
		return err
	}
	if shouldPatchControlPlaneReplicas {
		if err = kc.PatchControlPlaneReplicas(); err != nil {
			i.log.WithError(err).Error("Failed to patch control plane replicas")
			return err
		}
	}
	i.waitForMasterNodes(ctx, minMasterNodes, kc)
	if shouldPatchControlPlaneReplicas {
		if err = kc.UnPatchControlPlaneReplicas(); err != nil {
			i.log.WithError(err).Error("Failed to unPatch control plane replicas")
			return err
		}
	}
	return nil
}

func (i *installer) UpdateHostInstallProgress(newStage models.HostStage, info string) {
	ctx := utils.GenerateRequestContext()
	log := utils.RequestIDLogger(ctx, i.log)
	log.Infof("Updating node installation stage: %s - %s", newStage, info)
	if i.HostID != "" {
		if err := i.inventoryClient.UpdateHostInstallProgress(ctx, i.Config.InfraEnvID, i.Config.HostID, newStage, info); err != nil {
			log.Errorf("Failed to update node installation stage, %s", err)
		}
	}
}

func (i *installer) waitForBootkube(ctx context.Context) {
	i.log.Infof("Waiting for bootkube to complete")
	i.UpdateHostInstallProgress(models.HostStageWaitingForBootkube, "")

	for {
		select {
		case <-ctx.Done():
			i.log.Info("Context cancelled, terminating wait for bootkube\n")
			return
		case <-time.After(generalWaitInterval):
			// check if bootkube is done every 5 seconds
			if _, err := i.ops.ExecPrivilegeCommand(nil, "stat", "/opt/openshift/.bootkube.done"); err == nil {
				// in case bootkube is done log the status and return
				i.log.Info("bootkube service completed")
				out, _ := i.ops.ExecPrivilegeCommand(nil, "systemctl", "status", "bootkube.service")
				i.log.Info(out)
				return
			}
		}
	}
}

func (i *installer) waitForController(kc k8s_client.K8SClient) error {
	i.log.Infof("Waiting for controller to be ready")
	i.UpdateHostInstallProgress(models.HostStageWaitingForController, "waiting for controller pod ready event")

	events := map[string]string{}
	tickerUploadLogs := time.NewTicker(5 * time.Minute)
	tickerWaitForController := time.NewTicker(generalWaitInterval)
	for {
		select {
		case <-tickerWaitForController.C:
			if i.wasControllerReadyEventSet(kc, events) {
				i.log.Infof("Assisted controller is ready")
				i.inventoryClient.ClusterLogProgressReport(utils.GenerateRequestContext(), i.ClusterID, models.LogsStateRequested)
				i.uploadControllerLogs(kc)
				return nil
			}
		case <-tickerUploadLogs.C:
			i.uploadControllerLogs(kc)
		}
	}
}

func (i *installer) uploadControllerLogs(kc k8s_client.K8SClient) {
	controllerPod := common.GetPodInStatus(kc, common.AssistedControllerPrefix, assistedControllerNamespace,
		map[string]string{"job-name": common.AssistedControllerPrefix}, v1.PodRunning, i.log)
	if controllerPod != nil {
		//do not report the progress of this pre-fetching of controller logs to the service
		//since controller may not be ready at all and we'll end up waiting for a timeout to expire
		//in the service with no good reason before giving up on the logs
		//when controller is ready - it will report its log progress by itself
		err := common.UploadPodLogs(kc, i.inventoryClient, i.ClusterID, controllerPod.Name, assistedControllerNamespace, common.ControllerLogsSecondsAgo, i.log)
		// if failed to upload logs, log why and continue
		if err != nil {
			i.log.WithError(err).Warnf("Failed to upload controller logs")
		}
	}
}

func (i *installer) wasControllerReadyEventSet(kc k8s_client.K8SClient, previousEvents map[string]string) bool {
	newEvents, errEvents := kc.ListEvents(assistedControllerNamespace)
	if errEvents != nil {
		logrus.WithError(errEvents).Warnf("Failed to get controller events")
		return false
	}

	readyEventFound := false
	for _, event := range newEvents.Items {
		if _, ok := previousEvents[string(event.UID)]; !ok {
			i.log.Infof("Assisted controller new event: %s", event.Message)
			previousEvents[string(event.UID)] = event.Name
		}
		if event.Name == common.AssistedControllerIsReadyEvent {
			readyEventFound = true
		}
	}

	return readyEventFound
}

// wait for minimum master nodes to be in ready status
func (i *installer) waitForMasterNodes(ctx context.Context, minMasterNodes int, kc k8s_client.K8SClient) {

	var readyMasters []string
	var inventoryHostsMap map[string]inventory_client.HostData
	i.log.Infof("Waiting for %d master nodes", minMasterNodes)
	sufficientMasterNodes := func() bool {
		var err error
		inventoryHostsMap, err = i.getInventoryHostsMap(inventoryHostsMap)
		if err != nil {
			return false
		}
		nodes, err := kc.ListMasterNodes()
		if err != nil {
			i.log.Warnf("Still waiting for master nodes: %v", err)
			return false
		}
		if err = i.updateReadyMasters(nodes, &readyMasters, inventoryHostsMap); err != nil {
			i.log.WithError(err).Warnf("Failed to update ready with masters")
			return false
		}
		i.log.Infof("Found %d ready master nodes", len(readyMasters))
		if len(readyMasters) >= minMasterNodes {
			i.log.Infof("Waiting for master nodes - Done")
			return true
		}
		return false
	}

	for {
		select {
		case <-ctx.Done():
			i.log.Info("Context cancelled, terminating wait for master nodes\n")
			return
		case <-time.After(generalWaitInterval):
			// check if we have sufficient master nodes is done every 5 seconds
			if sufficientMasterNodes() {
				return
			}
		}
	}
}

func (i *installer) getInventoryHostsMap(hostsMap map[string]inventory_client.HostData) (map[string]inventory_client.HostData, error) {
	var err error
	if hostsMap == nil {
		ctx := utils.GenerateRequestContext()
		log := utils.RequestIDLogger(ctx, i.log)
		hostsMap, err = i.inventoryClient.GetEnabledHostsNamesHosts(ctx, log)
		if err != nil {
			log.Warnf("Failed to get hosts info from inventory, err %s", err)
			return nil, err
		}
		// no need for current host
		for name, hostData := range hostsMap {
			if hostData.Host.ID.String() == i.HostID {
				delete(hostsMap, name)
				break
			}
		}
	}
	return hostsMap, nil
}

func (i *installer) updateReadyMasters(nodes *v1.NodeList, readyMasters *[]string, inventoryHostsMap map[string]inventory_client.HostData) error {
	nodeNameAndCondition := map[string][]v1.NodeCondition{}
	knownIpAddresses := common.BuildHostsMapIPAddressBased(inventoryHostsMap)

	for _, node := range nodes.Items {
		nodeNameAndCondition[node.Name] = node.Status.Conditions
		if common.IsK8sNodeIsReady(node) && !funk.ContainsString(*readyMasters, node.Name) {
			ctx := utils.GenerateRequestContext()
			log := utils.RequestIDLogger(ctx, i.log)
			log.Infof("Found a new ready master node %s with id %s", node.Name, node.Status.NodeInfo.SystemUUID)
			*readyMasters = append(*readyMasters, node.Name)

			host, ok := common.HostMatchByNameOrIPAddress(node, inventoryHostsMap, knownIpAddresses)
			if !ok {
				return fmt.Errorf("Node %s is not in inventory hosts", node.Name)
			}
			ctx = utils.GenerateRequestContext()
			if err := i.inventoryClient.UpdateHostInstallProgress(ctx, host.Host.InfraEnvID.String(), host.Host.ID.String(), models.HostStageJoined, ""); err != nil {
				utils.RequestIDLogger(ctx, i.log).Errorf("Failed to update node installation status, %s", err)
			}
		}
	}

	i.log.Infof("Found %d master nodes: %+v", len(nodes.Items), nodeNameAndCondition)
	return nil
}

func (i *installer) cleanupInstallDevice() error {
	vgName, err := i.ops.GetVGByPV(i.Device)
	if err != nil {
		return err
	}

	if vgName == "" {
		i.log.Infof("No VG/LVM was found on device %s, no need to clean", i.Device)
		return nil
	}

	err = i.ops.RemoveVG(vgName)
	if err != nil {
		return err
	}

	_ = i.ops.Wipefs(i.Device)

	return i.ops.RemovePV(i.Device)
}

func (i *installer) verifyHostCanMoveToConfigurationStatus(inventoryHostsMapWithIp map[string]inventory_client.HostData) {
	logs, err := i.ops.GetMCSLogs()
	if err != nil {
		i.log.Infof("Failed to get MCS logs, will retry")
		return
	}
	common.SetConfiguringStatusForHosts(i.inventoryClient, inventoryHostsMapWithIp, logs, true, i.log)
}

func (i *installer) filterAlreadyUpdatedHosts(inventoryHostsMapWithIp map[string]inventory_client.HostData) {
	statesToFilter := map[models.HostStage]struct{}{models.HostStageConfiguring: {}, models.HostStageJoined: {},
		models.HostStageDone: {}, models.HostStageWaitingForIgnition: {}}
	for name, host := range inventoryHostsMapWithIp {
		fmt.Println(name, host.Host.Progress.CurrentStage)
		_, ok := statesToFilter[host.Host.Progress.CurrentStage]
		if ok {
			delete(inventoryHostsMapWithIp, name)
		}
	}
}

// will run as go routine and tries to find nodes that pulled ignition from mcs
// it will get mcs logs of static pod that runs on bootstrap and will search for matched ip
// when match is found it will update inventory service with new host status
func (i *installer) updateConfiguringStatus(ctx context.Context) {
	i.log.Infof("Start waiting for configuring state")
	ticker := time.NewTicker(generalWaitTimeout)
	var inventoryHostsMapWithIp map[string]inventory_client.HostData
	var err error
	for {
		select {
		case <-ctx.Done():
			i.log.Infof("Exiting updateConfiguringStatus go routine")
			return
		case <-ticker.C:
			i.log.Infof("searching for hosts that pulled ignition already")
			inventoryHostsMapWithIp, err = i.getInventoryHostsMap(inventoryHostsMapWithIp)
			if err != nil {
				continue
			}
			i.verifyHostCanMoveToConfigurationStatus(inventoryHostsMapWithIp)
			i.filterAlreadyUpdatedHosts(inventoryHostsMapWithIp)
			if len(inventoryHostsMapWithIp) == 0 {
				i.log.Infof("Exiting updateConfiguringStatus go routine")
				return
			}
		}
	}
}

// createSingleNodeMasterIgnition will start the bootstrap flow and wait for bootkube
// when bootkube complete the single node master ignition will be under singleNodeMasterIgnitionPath
func (i *installer) createSingleNodeMasterIgnition() (string, error) {
	if err := i.startBootstrap(); err != nil {
		i.log.Errorf("Bootstrap failed %s", err)
		return "", err
	}
	i.waitForBootkube(context.Background())
	_, err := i.ops.ExecPrivilegeCommand(utils.NewLogWriter(i.log), "stat", singleNodeMasterIgnitionPath)
	if err != nil {
		i.log.Errorf("Failed to find single node master ignition: %s", err)
		return "", err
	}
	i.Config.Role = string(models.HostRoleMaster)
	err = i.updateSingleNodeIgnition(singleNodeMasterIgnitionPath)
	if err != nil {
		return "", err
	}

	return singleNodeMasterIgnitionPath, nil
}

func (i *installer) checkLocalhostName() error {
	i.log.Infof("Start checking localhostname")
	hostname, err := i.ops.GetHostname()
	if err != nil {
		i.log.Errorf("Failed to get hostname from kernel, err %s157", err)
		return err
	}
	if hostname != "localhost" {
		i.log.Infof("hostname is not localhost, no need to do anything")
		return nil
	}

	data := fmt.Sprintf("random-hostname-%s", uuid.New().String())
	i.log.Infof("write data into /etc/hostname")
	return i.ops.CreateRandomHostname(data)
}
