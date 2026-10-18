/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"fmt"
	"regexp"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

var (
	digestImage = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)
	statusFile  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// ValidateTemplate re-checks the invariants the controller relies on. The CRD
// enforces them too; this guards against objects admitted by an older schema.
func ValidateTemplate(template *corev1alpha1.AgentHostTemplate) error {
	spec := &template.Spec
	if err := validateSettings(spec); err != nil {
		return err
	}
	return validateContainers(spec)
}

func validateSettings(spec *corev1alpha1.AgentHostTemplateSpec) error {
	if spec.SecurityContext.RunAsUser < 1 || spec.SecurityContext.RunAsGroup < 1 {
		return fmt.Errorf("securityContext must select a non-root uid and gid")
	}
	if spec.ListenPort < 1024 || spec.ListenPort > 65535 || spec.ListenPort == corev1alpha1.AgentHostStatusPort {
		return fmt.Errorf("listenPort %d is not allowed", spec.ListenPort)
	}
	switch spec.Access {
	case corev1alpha1.AgentHostAccessRelay:
		if spec.GitHubToken != corev1alpha1.AgentHostCredentialRequired {
			return fmt.Errorf("relay access requires githubToken: Required")
		}
	case corev1alpha1.AgentHostAccessStandalone:
	default:
		return fmt.Errorf("access %q is not supported", spec.Access)
	}
	switch spec.GitHubToken {
	case "", corev1alpha1.AgentHostCredentialNone, corev1alpha1.AgentHostCredentialOptional,
		corev1alpha1.AgentHostCredentialRequired:
	default:
		return fmt.Errorf("githubToken %q is not supported", spec.GitHubToken)
	}
	if spec.IdleStatusFile != "" && !statusFile.MatchString(spec.IdleStatusFile) {
		return fmt.Errorf("idleStatusFile must be a plain file name")
	}
	if spec.IdleSuspend != nil && (spec.IdleStatusFile == "" || spec.IdleSuspend.After.Duration <= 0) {
		return fmt.Errorf("idleSuspend requires idleStatusFile and a positive duration")
	}
	if spec.Storage.Size.Sign() <= 0 {
		return fmt.Errorf("storage size must be positive")
	}
	return nil
}

func validateContainers(spec *corev1alpha1.AgentHostTemplateSpec) error {
	scratch := map[string]bool{}
	for _, volume := range spec.Volumes {
		if volume.Name == corev1alpha1.AgentHostStateVolumeName || strings.HasPrefix(volume.Name, "orka-") {
			return fmt.Errorf("volume name %q is reserved", volume.Name)
		}
		scratch[volume.Name] = true
	}
	names := map[string]bool{}
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == corev1alpha1.AgentHostContainerName {
			return fmt.Errorf("an init container may not be named %s", corev1alpha1.AgentHostContainerName)
		}
		if err := validateContainer(&spec.InitContainers[i], names, scratch); err != nil {
			return err
		}
	}
	hosts := 0
	for i := range spec.Containers {
		if spec.Containers[i].Name == corev1alpha1.AgentHostContainerName {
			hosts++
		}
		if err := validateContainer(&spec.Containers[i], names, scratch); err != nil {
			return err
		}
	}
	if hosts != 1 {
		return fmt.Errorf("exactly one container must be named %s", corev1alpha1.AgentHostContainerName)
	}
	return nil
}

func validateContainer(container *corev1alpha1.AgentHostContainer, names, scratch map[string]bool) error {
	if strings.HasPrefix(container.Name, "orka-") || names[container.Name] {
		return fmt.Errorf("container name %q is reserved or repeated", container.Name)
	}
	names[container.Name] = true
	if !digestImage.MatchString(container.Image) {
		return fmt.Errorf("container %q image must be pinned by digest", container.Name)
	}
	for _, env := range container.Env {
		if strings.HasPrefix(env.Name, "ORKA_") {
			return fmt.Errorf("container %q sets reserved variable %s", container.Name, env.Name)
		}
	}
	for _, mount := range container.VolumeMounts {
		if mount.Name != corev1alpha1.AgentHostStateVolumeName && !scratch[mount.Name] {
			return fmt.Errorf("container %q mounts undeclared volume %q", container.Name, mount.Name)
		}
	}
	return nil
}
