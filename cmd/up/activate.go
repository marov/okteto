package up

import (
	"context"
	"fmt"
	"strings"

	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/pkg/analytics"
	"github.com/okteto/okteto/pkg/config"
	"github.com/okteto/okteto/pkg/errors"
	"github.com/okteto/okteto/pkg/k8s/deployments"
	okLabels "github.com/okteto/okteto/pkg/k8s/labels"
	"github.com/okteto/okteto/pkg/k8s/pods"
	"github.com/okteto/okteto/pkg/k8s/secrets"
	"github.com/okteto/okteto/pkg/k8s/services"
	"github.com/okteto/okteto/pkg/k8s/volumes"
	"github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/registry"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (up *upContext) activate(autoDeploy, build bool) error {
	log.Infof("activating development container retry=%t", up.isRetry)

	if err := config.UpdateStateFile(up.Dev, config.Activating); err != nil {
		return err
	}

	// create a new context on every iteration
	ctx, cancel := context.WithCancel(context.Background())
	up.Cancel = cancel
	up.ShutdownCompleted = make(chan bool, 1)
	up.Sy = nil
	up.Forwarder = nil
	defer up.shutdown()

	up.Disconnect = make(chan error, 1)
	up.CommandResult = make(chan error, 1)
	up.cleaned = make(chan string, 1)
	up.hardTerminate = make(chan error, 1)

	d, create, err := up.getCurrentDeployment(ctx, autoDeploy)
	if err != nil {
		return err
	}

	if up.isRetry && !deployments.IsDevModeOn(d) {
		log.Information("Development container has been deactivated")
		return nil
	}

	if deployments.IsDevModeOn(d) && deployments.HasBeenChanged(d) {
		return errors.UserError{
			E: fmt.Errorf("Deployment '%s' has been modified while your development container was active", d.Name),
			Hint: `Follow these steps:
	  1. Execute 'okteto down'
	  2. Apply your manifest changes again: 'kubectl apply'
	  3. Execute 'okteto up' again
    More information is available here: https://okteto.com/docs/reference/known-issues/index.html#kubectl-apply-changes-are-undone-by-okteto-up`,
		}
	}

	if _, err := registry.GetImageTagWithDigest(ctx, up.Dev.Namespace, up.Dev.Image.Name); err == errors.ErrNotFound {
		log.Infof("image '%s' not found, building it: %s", up.Dev.Image.Name, err.Error())
		build = true
	}

	if !up.isRetry && build {
		if err := up.buildDevImage(ctx, d, create); err != nil {
			return fmt.Errorf("error building dev image: %s", err)
		}
	}

	go up.initializeSyncthing()

	if err := up.setDevContainer(d); err != nil {
		return err
	}

	if err := up.devMode(ctx, d, create); err != nil {
		if errors.IsTransient(err) {
			return err
		}
		return fmt.Errorf("couldn't activate your development container\n    %s", err.Error())
	}

	up.isRetry = true

	if err := up.forwards(ctx); err != nil {
		if err == errors.ErrSSHConnectError {
			err := up.checkOktetoStartError(ctx, "Failed to connect to your development container")
			if err == errors.ErrLostSyncthing {
				if err := pods.Destroy(ctx, up.Pod.Name, up.Dev.Namespace, up.Client); err != nil {
					return fmt.Errorf("error recreating development container: %s", err.Error())
				}
			}
			return err
		}
		return fmt.Errorf("couldn't connect to your development container: %s", err.Error())
	}
	log.Success("Connected to your development container")

	go up.cleanCommand(ctx)

	if err := up.sync(ctx); err != nil {
		if up.shouldRetry(ctx, err) {
			return errors.ErrLostSyncthing
		}
		return err
	}

	up.success = true
	if up.isRetry {
		analytics.TrackReconnect(true, up.isSwap)
	}
	log.Success("Files synchronized")

	go func() {
		output := <-up.cleaned
		log.Debugf("clean command output: %s", output)

		outByCommand := strings.Split(strings.TrimSpace(output), "\n")
		if len(outByCommand) >= 2 {
			version, watches := outByCommand[0], outByCommand[1]

			if isWatchesConfigurationTooLow(watches) {
				folder := config.GetNamespaceHome(up.Dev.Namespace)
				if utils.GetWarningState(folder, ".remotewatcher") == "" {
					log.Yellow("The value of /proc/sys/fs/inotify/max_user_watches in your cluster nodes is too low.")
					log.Yellow("This can affect file synchronization performance.")
					log.Yellow("Visit https://okteto.com/docs/reference/known-issues/index.html for more information.")
					if err := utils.SetWarningState(folder, ".remotewatcher", "true"); err != nil {
						log.Infof("failed to set warning remotewatcher state: %s", err.Error())
					}
				}
			}

			if version != model.OktetoBinImageTag {
				log.Yellow("The Okteto CLI version %s uses the init container image %s.", config.VersionString, model.OktetoBinImageTag)
				log.Yellow("Please consider upgrading your init container image %s with the content of %s", up.Dev.InitContainer.Image, model.OktetoBinImageTag)
				log.Infof("Using init image %s instead of default init image (%s)", up.Dev.InitContainer.Image, model.OktetoBinImageTag)
			}
		}
		printDisplayContext(up.Dev)
		up.CommandResult <- up.runCommand(ctx)
	}()
	prevError := up.waitUntilExitOrInterrupt()

	if up.shouldRetry(ctx, prevError) {
		if !up.Dev.PersistentVolumeEnabled() {
			if err := pods.Destroy(ctx, up.Pod.Name, up.Dev.Namespace, up.Client); err != nil {
				return err
			}
		}
		return errors.ErrLostSyncthing
	}

	return prevError
}

func (up *upContext) shouldRetry(ctx context.Context, err error) bool {
	switch err {
	case nil:
		return false
	case errors.ErrLostSyncthing:
		return true
	case errors.ErrCommandFailed:
		return !up.Sy.Ping(ctx, false)
	}

	return false
}

func (up *upContext) devMode(ctx context.Context, d *appsv1.Deployment, create bool) error {
	if err := up.createDevContainer(ctx, d, create); err != nil {
		return err
	}
	log.Success("Development container activated")

	return up.waitUntilDevelopmentContainerIsRunning(ctx)
}

func (up *upContext) createDevContainer(ctx context.Context, d *appsv1.Deployment, create bool) error {
	spinner := utils.NewSpinner("Activating your development container...")
	spinner.Start()
	defer spinner.Stop()

	if err := config.UpdateStateFile(up.Dev, config.Starting); err != nil {
		return err
	}

	if up.Dev.PersistentVolumeEnabled() {
		if err := volumes.Create(ctx, up.Dev, up.Client); err != nil {
			return err
		}
	}

	trList, err := deployments.GetTranslations(ctx, up.Dev, d, up.Client)
	if err != nil {
		return err
	}

	if err := deployments.TranslateDevMode(trList, up.Client, up.isOktetoNamespace); err != nil {
		return err
	}

	initSyncErr := <-up.hardTerminate
	if initSyncErr != nil {
		return initSyncErr
	}

	log.Info("create deployment secrets")
	if err := secrets.Create(ctx, up.Dev, up.Client, up.Sy); err != nil {
		return err
	}

	for name := range trList {
		if name == d.Name {
			if err := deployments.Deploy(ctx, trList[name].Deployment, create, up.Client); err != nil {
				return err
			}
		} else {
			if err := deployments.Deploy(ctx, trList[name].Deployment, false, up.Client); err != nil {
				return err
			}
		}

		if trList[name].Deployment.Annotations[okLabels.DeploymentAnnotation] == "" {
			continue
		}

		if err := deployments.UpdateOktetoRevision(ctx, trList[name].Deployment, up.Client); err != nil {
			return err
		}

	}

	if create {
		if err := services.CreateDev(ctx, up.Dev, up.Client); err != nil {
			return err
		}
	}

	pod, err := pods.GetDevPodInLoop(ctx, up.Dev, up.Client, create)
	if err != nil {
		return err
	}

	up.Pod = pod
	return nil
}

func (up *upContext) waitUntilDevelopmentContainerIsRunning(ctx context.Context) error {
	msg := "Pulling images..."
	if up.Dev.PersistentVolumeEnabled() {
		msg = "Attaching persistent volume..."
		if err := config.UpdateStateFile(up.Dev, config.Attaching); err != nil {
			log.Infof("error updating state: %s", err.Error())
		}
	}

	spinner := utils.NewSpinner(msg)
	spinner.Start()
	defer spinner.Stop()

	optsWatchPod := metav1.ListOptions{
		Watch:         true,
		FieldSelector: fmt.Sprintf("metadata.name=%s", up.Pod.Name),
	}

	watcherPod, err := up.Client.CoreV1().Pods(up.Dev.Namespace).Watch(ctx, optsWatchPod)
	if err != nil {
		return err
	}

	optsWatchEvents := metav1.ListOptions{
		Watch:         true,
		FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", up.Pod.Name),
	}

	watcherEvents, err := up.Client.CoreV1().Events(up.Dev.Namespace).Watch(ctx, optsWatchEvents)
	if err != nil {
		return err
	}

	for {
		select {
		case event := <-watcherEvents.ResultChan():
			e, ok := event.Object.(*apiv1.Event)
			if !ok {
				watcherEvents, err = up.Client.CoreV1().Events(up.Dev.Namespace).Watch(ctx, optsWatchEvents)
				if err != nil {
					return err
				}
				continue
			}
			log.Infof("pod %s event: %s", up.Pod, e.Message)
			optsWatchEvents.ResourceVersion = e.ResourceVersion
			switch e.Reason {
			case "Failed", "FailedScheduling", "FailedCreatePodSandBox", "ErrImageNeverPull", "InspectFailed", "FailedCreatePodContainer":
				if strings.Contains(e.Message, "pod has unbound immediate PersistentVolumeClaims") {
					continue
				}
				return fmt.Errorf(e.Message)
			case "SuccessfulAttachVolume":
				spinner.Stop()
				log.Success("Persistent volume successfully attached")
				spinner.Update("Pulling images...")
				spinner.Start()
			case "Killing":
				return errors.ErrDevPodDeleted
			case "Pulling":
				message := strings.Replace(e.Message, "Pulling", "pulling", 1)
				spinner.Update(fmt.Sprintf("%s...", message))
				if err := config.UpdateStateFile(up.Dev, config.Pulling); err != nil {
					log.Infof("error updating state: %s", err.Error())
				}
			}
		case event := <-watcherPod.ResultChan():
			pod, ok := event.Object.(*apiv1.Pod)
			if !ok {
				watcherPod, err = up.Client.CoreV1().Pods(up.Dev.Namespace).Watch(ctx, optsWatchPod)
				if err != nil {
					return err
				}
				continue
			}
			log.Infof("dev pod %s is now %s", pod.Name, pod.Status.Phase)
			if pod.Status.Phase == apiv1.PodRunning {
				return nil
			}
			if pod.DeletionTimestamp != nil {
				return errors.ErrDevPodDeleted
			}
		case <-ctx.Done():
			log.Debug("call to waitUntilDevelopmentContainerIsRunning cancelled")
			return ctx.Err()
		}
	}
}
