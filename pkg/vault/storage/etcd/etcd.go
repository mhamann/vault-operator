/*
Copyright The KubeVault Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package etcd

import (
	"fmt"
	"path/filepath"
	"strings"

	api "kubevault.dev/operator/apis/kubevault/v1alpha1"

	core "k8s.io/api/core/v1"
)

const (
	// TLS related file name for etcd
	EtcdTLSAssetDir    = "/etc/vault/storage/etcd/tls/"
	EtcdClientCaName   = "ca.crt"
	EtcdClientCertName = "tls.crt"
	EtcdClientKeyName  = "tls.key"
)

var etcdStorageFmt = `
storage "etcd" {
%s
}
`

type Options struct {
	api.EtcdSpec
}

func NewOptions(s api.EtcdSpec) (*Options, error) {
	return &Options{
		s,
	}, nil
}

// Apply will do:
// - If TLSSecretName is provided, then add volume for etcd tls
// - If CredentialSecretName is provided, then set environment variable
func (o *Options) Apply(pt *core.PodTemplateSpec) error {
	etcdTLSAssetVolume := "vault-etcd-tls"
	if o.TLSSecretName != "" {
		// mount tls secret
		pt.Spec.Volumes = append(pt.Spec.Volumes, core.Volume{
			Name: etcdTLSAssetVolume,
			VolumeSource: core.VolumeSource{
				Secret: &core.SecretVolumeSource{
					SecretName: o.TLSSecretName,
				},
			},
		})

		pt.Spec.Containers[0].VolumeMounts = append(pt.Spec.Containers[0].VolumeMounts, core.VolumeMount{
			Name:      etcdTLSAssetVolume,
			MountPath: EtcdTLSAssetDir,
			ReadOnly:  true,
		})
	}

	if o.CredentialSecretName != "" {
		// set env variable ETCD_USERNAME and ETCD_PASSWORD
		pt.Spec.Containers[0].Env = append(pt.Spec.Containers[0].Env,
			core.EnvVar{
				Name: "ETCD_USERNAME",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{
							Name: o.CredentialSecretName,
						},
						Key: "username",
					},
				},
			},
			core.EnvVar{
				Name: "ETCD_PASSWORD",
				ValueFrom: &core.EnvVarSource{
					SecretKeyRef: &core.SecretKeySelector{
						LocalObjectReference: core.LocalObjectReference{
							Name: o.CredentialSecretName,
						},
						Key: "password",
					},
				},
			},
		)
	}

	return nil
}

// vault doc: https://www.vaultproject.io/docs/configuration/storage/etcd.html
//
// Note:
// - Secret `TLSSecretName` mounted in `EtcdTLSAssetDir`
// - Secret `CredentialSecret` will be used as environment variable
//
// GetStorageConfig creates etcd storage config from EtcdSpec
func (o *Options) GetStorageConfig() (string, error) {
	params := []string{}
	if o.Address != "" {
		params = append(params, fmt.Sprintf(`address = "%s"`, o.Address))
	}
	if o.EtcdApi != "" {
		params = append(params, fmt.Sprintf(`etcd_api = "%s"`, o.EtcdApi))
	}
	if o.Path != "" {
		params = append(params, fmt.Sprintf(`path = "%s"`, o.Path))
	}
	if o.DiscoverySrv != "" {
		params = append(params, fmt.Sprintf(`discovery_srv = "%s"`, o.DiscoverySrv))
	}
	if o.HAEnable {
		params = append(params, `ha_enabled = "true"`)
	} else {
		params = append(params, `ha_enabled = "false"`)
	}
	if o.Sync {
		params = append(params, `sync = "true"`)
	} else {
		params = append(params, `sync = "false"`)
	}
	if o.TLSSecretName != "" {
		params = append(params, fmt.Sprintf(`tls_ca_file = "%s"`, filepath.Join(EtcdTLSAssetDir, EtcdClientCaName)),
			fmt.Sprintf(`tls_cert_file = "%s"`, filepath.Join(EtcdTLSAssetDir, EtcdClientCertName)),
			fmt.Sprintf(`tls_key_file = "%s"`, filepath.Join(EtcdTLSAssetDir, EtcdClientKeyName)))
	}

	storageCfg := fmt.Sprintf(etcdStorageFmt, strings.Join(params, "\n"))
	return storageCfg, nil
}
