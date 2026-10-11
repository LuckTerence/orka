/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"math"
	"strings"
	"testing"
)

func TestHyperlightPodConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config HyperlightPodConfig
		want   string
	}{
		{"default", HyperlightPodConfig{}, ""},
		{"custom device", HyperlightPodConfig{DeviceResource: "example.com/kvm", DeviceGID: 990}, ""},
		{"maximum group", HyperlightPodConfig{DeviceGID: math.MaxInt32}, ""},
		{"negative group", HyperlightPodConfig{DeviceGID: -1}, "GID"},
		{"oversized group", HyperlightPodConfig{DeviceGID: int64(math.MaxInt32) + 1}, "GID"},
		{"uint32 group", HyperlightPodConfig{DeviceGID: math.MaxUint32}, "GID"},
		{"CPU", HyperlightPodConfig{DeviceResource: "cpu"}, "extended-resource"},
		{"memory", HyperlightPodConfig{DeviceResource: "memory"}, "extended-resource"},
		{"reserved domain", HyperlightPodConfig{DeviceResource: "kubernetes.io/kvm"}, "extended-resource"},
		{"reserved subdomain", HyperlightPodConfig{DeviceResource: "node.kubernetes.io/kvm"}, "extended-resource"},
		{"quota prefix", HyperlightPodConfig{DeviceResource: "requests.example.com/kvm"}, "extended-resource"},
		{"malformed name", HyperlightPodConfig{DeviceResource: "example.com/kvm/extra"}, "extended-resource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid config rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}
