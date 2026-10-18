/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testImage = "registry.example/host@sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"

func validTemplate() *corev1alpha1.AgentHostTemplate {
	return &corev1alpha1.AgentHostTemplate{
		Spec: corev1alpha1.AgentHostTemplateSpec{
			SecurityContext: corev1alpha1.AgentHostSecurityContext{RunAsUser: 1000, RunAsGroup: 1000},
			Containers: []corev1alpha1.AgentHostContainer{{
				Name: "host", Image: testImage,
				VolumeMounts: []corev1alpha1.AgentHostVolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}},
			}},
			Volumes:        []corev1alpha1.AgentHostScratchVolume{{Name: "tmp"}},
			Storage:        corev1alpha1.AgentHostStorage{Size: resource.MustParse("1Gi")},
			ListenPort:     7777,
			Access:         corev1alpha1.AgentHostAccessRelay,
			GitHubToken:    corev1alpha1.AgentHostCredentialRequired,
			IdleStatusFile: "idle.json",
		},
	}
}

func TestValidateTemplate(t *testing.T) {
	if err := ValidateTemplate(validTemplate()); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	cases := map[string]func(*corev1alpha1.AgentHostTemplateSpec){
		"no host container":   func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].Name = "daemon" },
		"two host containers": func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers = append(s.Containers, s.Containers[0]) },
		"unpinned image":      func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].Image = "registry.example/host:latest" },
		"reserved container":  func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].Name = "orka-host-agent" },
		"reserved env": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.Containers[0].Env = []corev1alpha1.AgentHostEnvVar{{Name: "ORKA_HOST_ID"}}
		},
		"undeclared mount":    func(s *corev1alpha1.AgentHostTemplateSpec) { s.Containers[0].VolumeMounts[1].Name = "cache" },
		"reserved volume":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.Volumes[0].Name = "orka-status" },
		"host-agent port":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.ListenPort = corev1alpha1.AgentHostStatusPort },
		"privileged port":     func(s *corev1alpha1.AgentHostTemplateSpec) { s.ListenPort = 80 },
		"relay without token": func(s *corev1alpha1.AgentHostTemplateSpec) { s.GitHubToken = corev1alpha1.AgentHostCredentialOptional },
		"root user":           func(s *corev1alpha1.AgentHostTemplateSpec) { s.SecurityContext.RunAsUser = 0 },
		"status file path":    func(s *corev1alpha1.AgentHostTemplateSpec) { s.IdleStatusFile = "../idle.json" },
		"empty storage":       func(s *corev1alpha1.AgentHostTemplateSpec) { s.Storage.Size = resource.MustParse("0") },
		"unknown access":      func(s *corev1alpha1.AgentHostTemplateSpec) { s.Access = "Tunnel" },
		"host as init": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.InitContainers = []corev1alpha1.AgentHostContainer{s.Containers[0]}
		},
		"suspend without status": func(s *corev1alpha1.AgentHostTemplateSpec) {
			s.IdleStatusFile = ""
			s.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: time.Minute}}
		},
	}
	for name, mutate := range cases {
		template := validTemplate()
		mutate(&template.Spec)
		if err := ValidateTemplate(template); err == nil {
			t.Errorf("%s: template accepted", name)
		}
	}
}

func TestParseIdleStatusIgnoresUnknownFieldsAndBoundsSize(t *testing.T) {
	status, err := ParseIdleStatus([]byte(`{"idle":true,"idleSince":"2026-10-17T21:29:40Z","extra":1}`))
	if err != nil || !status.Idle || status.IdleSince == nil {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	if _, err := ParseIdleStatus(make([]byte, maxStatusBytes+1)); err == nil {
		t.Fatal("oversized status accepted")
	}
}
