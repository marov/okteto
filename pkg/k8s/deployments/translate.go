// Copyright 2020 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deployments

import (
	"encoding/json"
	"fmt"
	"os"

	okLabels "github.com/okteto/okteto/pkg/k8s/labels"
	"github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/model"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	oktetoDeploymentAnnotation = "dev.okteto.com/deployment"
	oktetoVersionAnnotation    = "dev.okteto.com/version"
	revisionAnnotation         = "deployment.kubernetes.io/revision"
	//OktetoBinName name of the okteto bin init container
	OktetoBinName = "okteto-bin"

	//syncthing
	oktetoSyncSecretVolume = "okteto-sync-secret" // skipcq GSC-G101  not a secret
	oktetoDevSecretVolume  = "okteto-dev-secret"  // skipcq GSC-G101  not a secret
	oktetoSecretTemplate   = "okteto-%s"
)

var (
	devReplicas                      int32 = 1
	devTerminationGracePeriodSeconds int64
	falseBoolean                     = false

	//OktetoUpInitContainerRequestsCPU cpu requests used by the up init container
	OktetoUpInitContainerRequestsCPU = resource.MustParse("10m")
	//OktetoUpInitContainerRequestsMemory memory requests used by the up init container
	OktetoUpInitContainerRequestsMemory = resource.MustParse("10Mi")
	//OktetoUpInitContainerLimitsCPU cpu limits used by the up init container
	OktetoUpInitContainerLimitsCPU = resource.MustParse("30m")
	//OktetoUpInitContainerLimitsMemory limits requests used by the up init container
	OktetoUpInitContainerLimitsMemory = resource.MustParse("30Mi")
)

func translate(t *model.Translation, c *kubernetes.Clientset, isOktetoNamespace bool) error {
	for _, rule := range t.Rules {
		devContainer := GetDevContainer(&t.Deployment.Spec.Template.Spec, rule.Container)
		if devContainer == nil {
			return fmt.Errorf("Container '%s' not found in deployment '%s'", rule.Container, t.Deployment.Name)
		}
		rule.Container = devContainer.Name
	}

	manifest := getAnnotation(t.Deployment.GetObjectMeta(), oktetoDeploymentAnnotation)
	if manifest != "" {
		dOrig := &appsv1.Deployment{}
		if err := json.Unmarshal([]byte(manifest), dOrig); err != nil {
			return err
		}
		t.Deployment = dOrig
	}
	annotations := t.Deployment.GetObjectMeta().GetAnnotations()
	delete(annotations, revisionAnnotation)
	t.Deployment.GetObjectMeta().SetAnnotations(annotations)

	if c != nil && isOktetoNamespace {
		c := os.Getenv("OKTETO_CLIENTSIDE_TRANSLATION")
		if c == "" {
			commonTranslation(t)
			return setTranslationAsAnnotation(t.Deployment.Spec.Template.GetObjectMeta(), t)
		}

		log.Infof("using clientside translation")
	}

	t.Deployment.Status = appsv1.DeploymentStatus{}
	manifestBytes, err := json.Marshal(t.Deployment)
	if err != nil {
		return err
	}
	setAnnotation(t.Deployment.GetObjectMeta(), oktetoDeploymentAnnotation, string(manifestBytes))

	commonTranslation(t)
	setLabel(t.Deployment.Spec.Template.GetObjectMeta(), okLabels.DevLabel, "true")
	TranslateDevAnnotations(t.Deployment.Spec.Template.GetObjectMeta(), t.Annotations)
	TranslateDevTolerations(&t.Deployment.Spec.Template.Spec, t.Tolerations)
	t.Deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = &devTerminationGracePeriodSeconds

	if t.Interactive {
		TranslateOktetoSyncSecret(&t.Deployment.Spec.Template.Spec, t.Name)
	} else {
		TranslatePodAffinity(&t.Deployment.Spec.Template.Spec, t.Name)
	}
	for _, rule := range t.Rules {
		devContainer := GetDevContainer(&t.Deployment.Spec.Template.Spec, rule.Container)
		if devContainer == nil {
			return fmt.Errorf("Container '%s' not found in deployment '%s'", rule.Container, t.Deployment.Name)
		}

		TranslateDevContainer(devContainer, rule)
		TranslateInitContainer(&rule.InitContainer)
		TranslateOktetoVolumes(&t.Deployment.Spec.Template.Spec, rule)
		TranslatePodSecurityContext(&t.Deployment.Spec.Template.Spec, rule.SecurityContext)
		TranslatePodServiceAccount(&t.Deployment.Spec.Template.Spec, rule.ServiceAccount)
		TranslateOktetoDevSecret(&t.Deployment.Spec.Template.Spec, t.Name, rule.Secrets)
		if rule.IsMainDevContainer() {
			TranslateOktetoBinVolumeMounts(devContainer)
			TranslateOktetoInitBinContainer(rule.InitContainer, &t.Deployment.Spec.Template.Spec)
			TranslateOktetoBinVolume(&t.Deployment.Spec.Template.Spec)
		}
	}
	return nil
}

func commonTranslation(t *model.Translation) {
	TranslateDevAnnotations(t.Deployment.GetObjectMeta(), t.Annotations)
	setAnnotation(t.Deployment.GetObjectMeta(), oktetoVersionAnnotation, okLabels.Version)
	setLabel(t.Deployment.GetObjectMeta(), okLabels.DevLabel, "true")

	if t.Interactive {
		setLabel(t.Deployment.Spec.Template.GetObjectMeta(), okLabels.InteractiveDevLabel, t.Name)
	} else {
		setLabel(t.Deployment.Spec.Template.GetObjectMeta(), okLabels.DetachedDevLabel, t.Name)
	}

	t.Deployment.Spec.Replicas = &devReplicas
}

//GetDevContainer returns the dev container of a given deployment
func GetDevContainer(spec *apiv1.PodSpec, name string) *apiv1.Container {
	if name == "" {
		return &spec.Containers[0]
	}

	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}

	return nil
}

//TranslateDevAnnotations sets the user provided annotations
func TranslateDevAnnotations(o metav1.Object, annotations map[string]string) {
	for key, value := range annotations {
		setAnnotation(o, key, value)
	}
}

//TranslateDevTolerations sets the user provided toleretions
func TranslateDevTolerations(spec *apiv1.PodSpec, tolerations []apiv1.Toleration) {
	spec.Tolerations = append(spec.Tolerations, tolerations...)
}

//TranslatePodAffinity translates the affinity of pod to be all on the same node
func TranslatePodAffinity(spec *apiv1.PodSpec, name string) {
	if spec.Affinity == nil {
		spec.Affinity = &apiv1.Affinity{}
	}
	if spec.Affinity.PodAffinity == nil {
		spec.Affinity.PodAffinity = &apiv1.PodAffinity{}
	}
	if spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = []apiv1.PodAffinityTerm{}
	}
	spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		apiv1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					okLabels.InteractiveDevLabel: name,
				},
			},
			TopologyKey: "kubernetes.io/hostname",
		},
	)
}

//TranslateDevContainer translates a dev container
func TranslateDevContainer(c *apiv1.Container, rule *model.TranslationRule) {
	if rule.Image == "" {
		rule.Image = c.Image
	}
	c.Image = rule.Image
	c.ImagePullPolicy = rule.ImagePullPolicy

	if rule.WorkDir != "" {
		c.WorkingDir = rule.WorkDir
	}

	if len(rule.Command) > 0 {
		c.Command = rule.Command
		c.Args = rule.Args
	}

	TranslateProbes(c, *rule.Probes)

	TranslateResources(c, rule.Resources)
	TranslateEnvVars(c, rule)
	TranslateVolumeMounts(c, rule)
	TranslateContainerSecurityContext(c, rule.SecurityContext)
}

//TranslateProbes translates the healthchecks attached to a container
func TranslateProbes(c *apiv1.Container, h model.Probes) {
	if !h.Liveness {
		c.LivenessProbe = nil
	}
	if !h.Readiness {
		c.ReadinessProbe = nil
	}
	if !h.Startup {
		c.StartupProbe = nil
	}
}

func TranslateInitContainer(initContainer *model.InitContainer) {
	if initContainer.Resources.Limits == nil {
		initContainer.Resources.Limits = make(map[apiv1.ResourceName]resource.Quantity)
	}
	if initContainer.Resources.Requests == nil {
		initContainer.Resources.Requests = make(map[apiv1.ResourceName]resource.Quantity)
	}
	setDefaultResourceValueIfNotPresent(initContainer.Resources.Limits, apiv1.ResourceMemory, OktetoUpInitContainerLimitsMemory)
	setDefaultResourceValueIfNotPresent(initContainer.Resources.Limits, apiv1.ResourceCPU, OktetoUpInitContainerLimitsCPU)

	setDefaultResourceValueIfNotPresent(initContainer.Resources.Requests, apiv1.ResourceMemory, OktetoUpInitContainerRequestsMemory)
	setDefaultResourceValueIfNotPresent(initContainer.Resources.Requests, apiv1.ResourceCPU, OktetoUpInitContainerRequestsCPU)
}

func setDefaultResourceValueIfNotPresent(resourceList model.ResourceList, resourceName apiv1.ResourceName, value resource.Quantity) {
	if _, ok := resourceList[resourceName]; !ok {
		resourceList[resourceName] = value
	}
}

//TranslateResources translates the resources attached to a container
func TranslateResources(c *apiv1.Container, r model.ResourceRequirements) {
	if c.Resources.Requests == nil {
		c.Resources.Requests = make(map[apiv1.ResourceName]resource.Quantity)
	}

	if v, ok := r.Requests[apiv1.ResourceMemory]; ok {
		c.Resources.Requests[apiv1.ResourceMemory] = v
	}

	if v, ok := r.Requests[apiv1.ResourceCPU]; ok {
		c.Resources.Requests[apiv1.ResourceCPU] = v
	}

	if v, ok := r.Requests[model.ResourceAMDGPU]; ok {
		c.Resources.Requests[model.ResourceAMDGPU] = v
	}

	if v, ok := r.Requests[model.ResourceNVIDIAGPU]; ok {
		c.Resources.Requests[model.ResourceNVIDIAGPU] = v
	}

	if c.Resources.Limits == nil {
		c.Resources.Limits = make(map[apiv1.ResourceName]resource.Quantity)
	}

	if v, ok := r.Limits[apiv1.ResourceMemory]; ok {
		c.Resources.Limits[apiv1.ResourceMemory] = v
	}

	if v, ok := r.Limits[apiv1.ResourceCPU]; ok {
		c.Resources.Limits[apiv1.ResourceCPU] = v
	}

	if v, ok := r.Limits[model.ResourceAMDGPU]; ok {
		c.Resources.Limits[model.ResourceAMDGPU] = v
	}

	if v, ok := r.Limits[model.ResourceNVIDIAGPU]; ok {
		c.Resources.Limits[model.ResourceNVIDIAGPU] = v
	}
}

//TranslateEnvVars translates the variables attached to a container
func TranslateEnvVars(c *apiv1.Container, rule *model.TranslationRule) {
	unusedDevEnvVar := map[string]string{}
	for _, val := range rule.Environment {
		unusedDevEnvVar[val.Name] = val.Value
	}
	for i, envvar := range c.Env {
		if value, ok := unusedDevEnvVar[envvar.Name]; ok {
			c.Env[i] = apiv1.EnvVar{Name: envvar.Name, Value: value}
			delete(unusedDevEnvVar, envvar.Name)
		}
	}
	for _, envvar := range rule.Environment {
		if value, ok := unusedDevEnvVar[envvar.Name]; ok {
			c.Env = append(c.Env, apiv1.EnvVar{Name: envvar.Name, Value: value})
		}
	}
}

//TranslateVolumeMounts translates the volumes attached to a container
func TranslateVolumeMounts(c *apiv1.Container, rule *model.TranslationRule) {
	if c.VolumeMounts == nil {
		c.VolumeMounts = []apiv1.VolumeMount{}
	}

	for _, v := range rule.Volumes {
		c.VolumeMounts = append(
			c.VolumeMounts,
			apiv1.VolumeMount{
				Name:      v.Name,
				MountPath: v.MountPath,
				SubPath:   v.SubPath,
			},
		)
	}

	if rule.Marker == "" {
		return
	}
	c.VolumeMounts = append(
		c.VolumeMounts,
		apiv1.VolumeMount{
			Name:      oktetoSyncSecretVolume,
			MountPath: "/var/syncthing/secret/",
		},
	)
	if len(rule.Secrets) > 0 {
		c.VolumeMounts = append(
			c.VolumeMounts,
			apiv1.VolumeMount{
				Name:      oktetoDevSecretVolume,
				MountPath: "/var/okteto/secret/",
			},
		)
	}
}

//TranslateOktetoBinVolumeMounts translates the binaries mount attached to a container
func TranslateOktetoBinVolumeMounts(c *apiv1.Container) {
	if c.VolumeMounts == nil {
		c.VolumeMounts = []apiv1.VolumeMount{}
	}
	for _, vm := range c.VolumeMounts {
		if vm.Name == OktetoBinName {
			return
		}
	}
	vm := apiv1.VolumeMount{
		Name:      OktetoBinName,
		MountPath: "/var/okteto/bin",
	}
	c.VolumeMounts = append(c.VolumeMounts, vm)
}

//TranslateOktetoVolumes translates the dev volumes
func TranslateOktetoVolumes(spec *apiv1.PodSpec, rule *model.TranslationRule) {
	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}

	for _, rV := range rule.Volumes {
		found := false
		for i := range spec.Volumes {
			if spec.Volumes[i].Name == rV.Name {
				found = true
				break
			}
		}
		if found {
			continue
		}

		v := apiv1.Volume{
			Name:         rV.Name,
			VolumeSource: apiv1.VolumeSource{},
		}

		if !rule.PersistentVolume && rV.IsSyncthing() {
			v.VolumeSource.EmptyDir = &apiv1.EmptyDirVolumeSource{}
		} else {
			v.VolumeSource.PersistentVolumeClaim = &apiv1.PersistentVolumeClaimVolumeSource{
				ClaimName: rV.Name,
				ReadOnly:  false,
			}
		}

		spec.Volumes = append(spec.Volumes, v)
	}
}

//TranslateOktetoBinVolume translates the binaries volume attached to a container
func TranslateOktetoBinVolume(spec *apiv1.PodSpec) {
	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == OktetoBinName {
			return
		}
	}

	v := apiv1.Volume{
		Name: OktetoBinName,
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
	spec.Volumes = append(spec.Volumes, v)
}

//TranslatePodSecurityContext translates the security context attached to a pod
func TranslatePodSecurityContext(spec *apiv1.PodSpec, s *model.SecurityContext) {
	if s == nil {
		return
	}

	if spec.SecurityContext == nil {
		spec.SecurityContext = &apiv1.PodSecurityContext{}
	}

	if s.FSGroup != nil {
		spec.SecurityContext.FSGroup = s.FSGroup
	}
}

//TranslatePodServiceAccount translates the security accout the pod uses
func TranslatePodServiceAccount(spec *apiv1.PodSpec, sa string) {
	if sa != "" {
		spec.ServiceAccountName = sa
	}
}

//TranslateContainerSecurityContext translates the security context attached to a container
func TranslateContainerSecurityContext(c *apiv1.Container, s *model.SecurityContext) {
	if s == nil {
		return
	}

	if c.SecurityContext == nil {
		c.SecurityContext = &apiv1.SecurityContext{}
	}

	if s.RunAsUser != nil {
		c.SecurityContext.RunAsUser = s.RunAsUser
		if *s.RunAsUser == 0 {
			c.SecurityContext.RunAsNonRoot = &falseBoolean
		}
	}

	if s.RunAsGroup != nil {
		c.SecurityContext.RunAsGroup = s.RunAsGroup
		if *s.RunAsGroup == 0 {
			c.SecurityContext.RunAsNonRoot = &falseBoolean
		}
	}

	if s.Capabilities == nil {
		return
	}
	if c.SecurityContext.Capabilities == nil {
		c.SecurityContext.Capabilities = &apiv1.Capabilities{}
	}

	c.SecurityContext.ReadOnlyRootFilesystem = nil
	c.SecurityContext.Capabilities.Add = append(c.SecurityContext.Capabilities.Add, s.Capabilities.Add...)
	c.SecurityContext.Capabilities.Drop = append(c.SecurityContext.Capabilities.Drop, s.Capabilities.Drop...)
}

//TranslateOktetoInitBinContainer translates the bin init container of a pod
func TranslateOktetoInitBinContainer(initContainer model.InitContainer, spec *apiv1.PodSpec) {

	c := apiv1.Container{
		Name:            OktetoBinName,
		Image:           initContainer.Image,
		ImagePullPolicy: apiv1.PullIfNotPresent,
		Command:         []string{"sh", "-c", "cp /usr/local/bin/* /okteto/bin"},
		VolumeMounts: []apiv1.VolumeMount{
			{
				Name:      OktetoBinName,
				MountPath: "/okteto/bin",
			},
		},
		Resources: apiv1.ResourceRequirements{
			Requests: map[apiv1.ResourceName]resource.Quantity{
				apiv1.ResourceCPU:    initContainer.Resources.Requests[apiv1.ResourceCPU],
				apiv1.ResourceMemory: initContainer.Resources.Requests[apiv1.ResourceMemory],
			},
			Limits: map[apiv1.ResourceName]resource.Quantity{
				apiv1.ResourceCPU:    initContainer.Resources.Limits[apiv1.ResourceCPU],
				apiv1.ResourceMemory: initContainer.Resources.Limits[apiv1.ResourceMemory],
			},
		},
	}

	if spec.InitContainers == nil {
		spec.InitContainers = []apiv1.Container{}
	}
	spec.InitContainers = append(spec.InitContainers, c)
}

//TranslateOktetoSyncSecret translates the syncthing secret container of a pod
func TranslateOktetoSyncSecret(spec *apiv1.PodSpec, name string) {
	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == oktetoSyncSecretVolume {
			return
		}
	}

	var mode int32 = 0444
	v := apiv1.Volume{
		Name: oktetoSyncSecretVolume,
		VolumeSource: apiv1.VolumeSource{
			Secret: &apiv1.SecretVolumeSource{
				SecretName: fmt.Sprintf(oktetoSecretTemplate, name),
				Items: []apiv1.KeyToPath{
					{
						Key:  "config.xml",
						Path: "config.xml",
						Mode: &mode,
					},
					{
						Key:  "cert.pem",
						Path: "cert.pem",
						Mode: &mode,
					},
					{
						Key:  "key.pem",
						Path: "key.pem",
						Mode: &mode,
					},
				},
			},
		},
	}
	spec.Volumes = append(spec.Volumes, v)
}

//TranslateOktetoDevSecret translates the devs secret of a pod
func TranslateOktetoDevSecret(spec *apiv1.PodSpec, secret string, secrets []model.Secret) {
	if len(secrets) == 0 {
		return
	}

	if spec.Volumes == nil {
		spec.Volumes = []apiv1.Volume{}
	}
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == oktetoDevSecretVolume {
			return
		}
	}
	v := apiv1.Volume{
		Name: oktetoDevSecretVolume,
		VolumeSource: apiv1.VolumeSource{
			Secret: &apiv1.SecretVolumeSource{
				SecretName: fmt.Sprintf(oktetoSecretTemplate, secret),
				Items:      []apiv1.KeyToPath{},
			},
		},
	}
	for i, s := range secrets {
		v.VolumeSource.Secret.Items = append(
			v.VolumeSource.Secret.Items,
			apiv1.KeyToPath{
				Key:  s.GetKeyName(),
				Path: s.GetFileName(),
				Mode: &secrets[i].Mode,
			},
		)
	}
	spec.Volumes = append(spec.Volumes, v)
}
