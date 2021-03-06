// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/adapter/config/crd"
	"istio.io/istio/pilot/model"
	"istio.io/istio/pilot/platform"
	"istio.io/istio/pilot/platform/kube/inject"
	"istio.io/istio/pilot/test/util"
	"istio.io/istio/pkg/log"
)

const (
	ingressSecretName = "istio-ingress-certs"
)

type infra struct { // nolint: maligned
	Name string

	// docker tags
	Hub, Tag string

	Namespace      string
	IstioNamespace string
	Registry       string
	Verbosity      int

	// map from app to pods
	apps map[string][]string

	Auth                   meshconfig.MeshConfig_AuthPolicy
	ControlPlaneAuthPolicy meshconfig.AuthenticationPolicy
	MixerCustomConfigFile  string
	PilotCustomConfigFile  string

	// switches for infrastructure components
	Mixer     bool
	Ingress   bool
	Zipkin    bool
	DebugPort int

	SkipCleanup          bool
	SkipCleanupOnFailure bool

	// check proxy logs
	checkLogs bool

	// store error logs in specific directory
	errorLogsDir string

	namespaceCreated      bool
	istioNamespaceCreated bool
	debugImagesAndMode    bool

	// sidecar initializer
	UseInitializer bool
	InjectConfig   *inject.Config

	// External Admission Webhook for validation
	UseAdmissionWebhook  bool
	AdmissionServiceName string

	config model.IstioConfigStore
}

func (infra *infra) setup() error {
	crdclient, crderr := crd.NewClient(kubeconfig, model.IstioConfigTypes, "")
	if crderr != nil {
		return crderr
	}
	if err := crdclient.RegisterResources(); err != nil {
		return err
	}

	infra.config = model.MakeIstioStore(crdclient)

	if infra.Namespace == "" {
		var err error
		if infra.Namespace, err = util.CreateNamespaceWithPrefix(client, "istio-test-app-"); err != nil {
			return err
		}
		infra.namespaceCreated = true
	} else {
		if _, err := client.CoreV1().Namespaces().Get(infra.Namespace, meta_v1.GetOptions{}); err != nil {
			return err
		}
	}

	if infra.IstioNamespace == "" {
		var err error
		if infra.IstioNamespace, err = util.CreateNamespaceWithPrefix(client, "istio-test-"); err != nil {
			return err
		}
		infra.istioNamespaceCreated = true
	} else {
		if _, err := client.CoreV1().Namespaces().Get(infra.IstioNamespace, meta_v1.GetOptions{}); err != nil {
			return err
		}
	}

	deploy := func(name, namespace string) error {
		if yaml, err := fill(name, infra); err != nil {
			return err
		} else if err = infra.kubeApply(yaml, namespace); err != nil {
			return err
		}
		return nil
	}
	if err := deploy("rbac-beta.yaml.tmpl", infra.IstioNamespace); err != nil {
		return err
	}

	if err := deploy("config.yaml.tmpl", infra.IstioNamespace); err != nil {
		return err
	}

	_, mesh, err := inject.GetMeshConfig(client, infra.IstioNamespace, "istio")
	if err != nil {
		return err
	}
	debugMode := infra.debugImagesAndMode
	log.Infof("mesh %s", spew.Sdump(mesh))

	// Default to NamespaceAll to mirror kube-inject behavior. Only
	// use a specific include namespace for the automatic injection.
	includeNamespaces := []string{v1.NamespaceAll}
	if infra.UseInitializer {
		includeNamespaces = []string{infra.Namespace}
	}

	infra.InjectConfig = &inject.Config{
		Policy:            inject.InjectionPolicyEnabled,
		IncludeNamespaces: includeNamespaces,
		Params: inject.Params{
			InitImage:       inject.InitImageName(infra.Hub, infra.Tag, debugMode),
			ProxyImage:      inject.ProxyImageName(infra.Hub, infra.Tag, debugMode),
			Verbosity:       infra.Verbosity,
			SidecarProxyUID: inject.DefaultSidecarProxyUID,
			EnableCoreDump:  true,
			Version:         "integration-test",
			Mesh:            mesh,
			DebugMode:       debugMode,
		},
	}

	if infra.UseInitializer {
		if err := deploy("initializer-config.yaml.tmpl", infra.IstioNamespace); err != nil {
			return err
		}
		if yaml, err := fill("initializer-configmap.yaml.tmpl", &infra.InjectConfig); err != nil {
			return err
		} else if err = infra.kubeApply(yaml, infra.IstioNamespace); err != nil {
			return err
		}
		if err := deploy("initializer.yaml.tmpl", infra.IstioNamespace); err != nil {
			return err
		}
		// InitializerConfiguration will block *all* deployments and
		// could possibly lead to timeouts when trying to create other
		// Istio runtime components. Wait until it's pod is ready
		// before proceeding with the test setup.
		if _, err = util.GetAppPods(client, kubeconfig, []string{infra.IstioNamespace}); err != nil {
			return fmt.Errorf("initialized failed to start: %v", err)
		}
	}

	if infra.UseAdmissionWebhook {
		if err := infra.createAdmissionWebhookSecret(); err != nil {
			return err
		}
	}

	if err := deploy("pilot.yaml.tmpl", infra.IstioNamespace); err != nil {
		return err
	}
	if infra.Mixer {
		if err := deploy("mixer.yaml.tmpl", infra.IstioNamespace); err != nil {
			return err
		}
	}
	if platform.ServiceRegistry(infra.Registry) == platform.EurekaRegistry {
		if err := deploy("eureka.yaml.tmpl", infra.IstioNamespace); err != nil {
			return err
		}
	}

	if err := deploy("ca.yaml.tmpl", infra.IstioNamespace); err != nil {
		return err
	}
	if err := deploy("headless.yaml.tmpl", infra.Namespace); err != nil {
		return err
	}
	if infra.Ingress {
		if err := deploy("ingress-proxy.yaml.tmpl", infra.IstioNamespace); err != nil {
			return err
		}
		// Create ingress key/cert in secret
		key, err := ioutil.ReadFile("pilot/docker/certs/cert.key")
		if err != nil {
			return err
		}
		crt, err := ioutil.ReadFile("pilot/docker/certs/cert.crt")
		if err != nil {
			return err
		}
		_, err = client.CoreV1().Secrets(infra.IstioNamespace).Create(&v1.Secret{
			ObjectMeta: meta_v1.ObjectMeta{Name: ingressSecretName},
			Data: map[string][]byte{
				"tls.key": key,
				"tls.crt": crt,
			},
		})
		if err != nil {
			return err
		}
	}
	if infra.Zipkin {
		if err := deploy("zipkin.yaml", infra.IstioNamespace); err != nil {
			return err
		}
	}

	return nil
}

func (infra *infra) deployApps() error {
	// deploy a healthy mix of apps, with and without proxy
	if err := infra.deployApp("t", "t", 8080, 80, 9090, 90, 7070, 70, "unversioned", false, false); err != nil {
		return err
	}
	if err := infra.deployApp("a", "a", 8080, 80, 9090, 90, 7070, 70, "v1", true, false); err != nil {
		return err
	}
	if err := infra.deployApp("b", "b", 80, 8080, 90, 9090, 70, 7070, "unversioned", true, false); err != nil {
		return err
	}
	if err := infra.deployApp("c-v1", "c", 80, 8080, 90, 9090, 70, 7070, "v1", true, false); err != nil {
		return err
	}
	if err := infra.deployApp("c-v2", "c", 80, 8080, 90, 9090, 70, 7070, "v2", true, false); err != nil {
		return err
	}
	if err := infra.deployApp("d", "d", 80, 8080, 90, 9090, 70, 7070, "per-svc-auth", true, true); err != nil {
		return err
	}
	// Add another service without sidecar to test mTLS blacklisting (as in the e2e test
	// environment, pilot can see only services in the test namespaces). This service
	// will be listed in mtlsExcludedServices in the mesh config.
	return infra.deployApp("e", "fake-control", 80, 8080, 90, 9090, 70, 7070, "fake-control", false, false)
}

func (infra *infra) deployApp(deployment, svcName string, port1, port2, port3, port4, port5, port6 int,
	version string, injectProxy bool, perServiceAuth bool) error {
	// Eureka does not support management ports
	healthPort := "true"
	if platform.ServiceRegistry(infra.Registry) == platform.EurekaRegistry {
		healthPort = "false"
	}

	w, err := fill("app.yaml.tmpl", map[string]string{
		"Hub":            infra.Hub,
		"Tag":            infra.Tag,
		"service":        svcName,
		"perServiceAuth": strconv.FormatBool(perServiceAuth),
		"deployment":     deployment,
		"port1":          strconv.Itoa(port1),
		"port2":          strconv.Itoa(port2),
		"port3":          strconv.Itoa(port3),
		"port4":          strconv.Itoa(port4),
		"port5":          strconv.Itoa(port5),
		"port6":          strconv.Itoa(port6),
		"version":        version,
		"istioNamespace": infra.IstioNamespace,
		"injectProxy":    strconv.FormatBool(injectProxy),
		"healthPort":     healthPort,
	})
	if err != nil {
		return err
	}

	writer := new(bytes.Buffer)

	if injectProxy && !infra.UseInitializer {
		if err := inject.IntoResourceFile(infra.InjectConfig, strings.NewReader(w), writer); err != nil {
			return err
		}
	} else {
		if _, err := io.Copy(writer, strings.NewReader(w)); err != nil {
			return err
		}
	}

	return infra.kubeApply(writer.String(), infra.Namespace)
}

func (infra *infra) teardown() {
	if yaml, err := fill("rbac-beta.yaml.tmpl", infra); err != nil {
		log.Infof("RBAC template could could not be processed, please delete stale ClusterRoleBindings: %v",
			err)
	} else if err = infra.kubeDelete(yaml, infra.IstioNamespace); err != nil {
		log.Infof("RBAC config could could not be deleted: %v", err)
	}

	if infra.UseAdmissionWebhook {
		if err := infra.deleteAdmissionWebhookSecret(); err != nil {
			log.Infof("Could not delete admission webhook secret: %v", err)
		}
	}

	if infra.namespaceCreated {
		util.DeleteNamespace(client, infra.Namespace)
		infra.Namespace = ""
	}
	if infra.istioNamespaceCreated {
		util.DeleteNamespace(client, infra.IstioNamespace)
		infra.IstioNamespace = ""
	}

	// InitializerConfiguration is not namespaced.
	if infra.UseInitializer {
		if yaml, err := fill("initializer-config.yaml.tmpl", infra); err != nil {
			log.Infof("Sidecar initializer configuration could not be processed, "+
				"please delete stale InitializerConfiguration : %v", err)
		} else if err := infra.kubeDelete(yaml, infra.IstioNamespace); err != nil {
			log.Infof("Sidecar initializer configuration could not be deleted: %v", err)
		}
	}
}

func (infra *infra) kubeApply(yaml, namespace string) error {
	return util.RunInput(fmt.Sprintf("kubectl apply --kubeconfig %s -n %s -f -",
		kubeconfig, namespace), yaml)
}

func (infra *infra) kubeDelete(yaml, namespace string) error {
	return util.RunInput(fmt.Sprintf("kubectl delete --kubeconfig %s -n %s -f -",
		kubeconfig, namespace), yaml)
}

type response struct {
	body    string
	id      []string
	version []string
	port    []string
	code    []string
}

const httpOk = "200"

var (
	idRex      = regexp.MustCompile("(?i)X-Request-Id=(.*)")
	versionRex = regexp.MustCompile("ServiceVersion=(.*)")
	portRex    = regexp.MustCompile("ServicePort=(.*)")
	codeRex    = regexp.MustCompile("StatusCode=(.*)")
)

func (infra *infra) clientRequest(app, url string, count int, extra string) response {
	out := response{}
	if len(infra.apps[app]) == 0 {
		log.Errorf("missing pod names for app %q", app)
		return out
	}

	pod := infra.apps[app][0]
	cmd := fmt.Sprintf("kubectl exec %s --kubeconfig %s -n %s -c app -- client -url %s -count %d %s",
		pod, kubeconfig, infra.Namespace, url, count, extra)
	request, err := util.Shell(cmd)

	if err != nil {
		log.Errorf("client request error %v for %s in %s", err, url, app)
		return out
	}

	out.body = request

	ids := idRex.FindAllStringSubmatch(request, -1)
	for _, id := range ids {
		out.id = append(out.id, id[1])
	}

	versions := versionRex.FindAllStringSubmatch(request, -1)
	for _, version := range versions {
		out.version = append(out.version, version[1])
	}

	ports := portRex.FindAllStringSubmatch(request, -1)
	for _, port := range ports {
		out.port = append(out.port, port[1])
	}

	codes := codeRex.FindAllStringSubmatch(request, -1)
	for _, code := range codes {
		out.code = append(out.code, code[1])
	}

	return out
}

func (infra *infra) applyConfig(inFile string, data map[string]string) error {
	config, err := fill(inFile, data)
	if err != nil {
		return err
	}

	vs, _, err := crd.ParseInputs(config)
	if err != nil {
		return err
	}

	for _, v := range vs {
		// fill up namespace for the config
		v.Namespace = infra.Namespace

		old, exists := infra.config.Get(v.Type, v.Name, v.Namespace)
		if exists {
			v.ResourceVersion = old.ResourceVersion
			_, err = infra.config.Update(v)
		} else {
			_, err = infra.config.Create(v)
		}
		if err != nil {
			return err
		}
	}

	sleepTime := time.Second * 3
	log.Infof("Sleeping %v for the config to propagate", sleepTime)
	time.Sleep(sleepTime)
	return nil
}

func (infra *infra) deleteConfig(inFile string) error {
	config, err := fill(inFile, nil)
	if err != nil {
		return err
	}

	vs, _, err := crd.ParseInputs(config)
	if err != nil {
		return err
	}

	for _, v := range vs {
		// fill up namespace for the config
		v.Namespace = infra.Namespace

		log.Infof("Delete config %s", v.Key())
		if err = infra.config.Delete(v.Type, v.Name, v.Namespace); err != nil {
			return err
		}
	}

	sleepTime := time.Second * 3
	log.Infof("Sleeping %v for the config to propagate", sleepTime)
	time.Sleep(sleepTime)
	return nil
}

func (infra *infra) deleteAllConfigs() error {
	for _, desc := range infra.config.ConfigDescriptor() {
		configs, err := infra.config.List(desc.Type, infra.Namespace)
		if err != nil {
			return err
		}
		for _, config := range configs {
			log.Infof("Delete config %s", config.Key())
			if err = infra.config.Delete(desc.Type, config.Name, config.Namespace); err != nil {
				return err
			}
		}
	}
	return nil
}

func createWebhookCerts(service, namespace string) (caCertPEM, serverCertPEM, serverKeyPEM []byte, err error) { // nolint: lll
	var (
		webhookCertValidFor = 365 * 24 * time.Hour
		rsaBits             = 2048
		maxSerialNumber     = new(big.Int).Lsh(big.NewInt(1), 128)

		notBefore = time.Now()
		notAfter  = notBefore.Add(webhookCertValidFor)
	)

	// Generate self-signed CA cert
	caKey, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, nil, nil, err
	}
	caSerialNumber, err := rand.Int(rand.Reader, maxSerialNumber)
	if err != nil {
		return nil, nil, nil, err
	}
	caTemplate := x509.Certificate{
		SerialNumber:          caSerialNumber,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("%s_a", service)},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA: true,
	}
	caCert, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	// Generate server certificate signed by self-signed CA
	serverKey, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, nil, nil, err
	}
	serverSerialNumber, err := rand.Int(rand.Reader, maxSerialNumber)
	if err != nil {
		return nil, nil, nil, err
	}
	serverTemplate := x509.Certificate{
		SerialNumber: serverSerialNumber,
		Subject:      pkix.Name{CommonName: fmt.Sprintf("%s.%s.svc", service, namespace)},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	serverCert, err := x509.CreateCertificate(rand.Reader, &serverTemplate, &caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	// PEM encoding
	caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert})
	serverCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCert})
	serverKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})

	return caCertPEM, serverCertPEM, serverKeyPEM, nil
}

func (infra *infra) createAdmissionWebhookSecret() error {
	caCert, serverCert, serverKey, err := createWebhookCerts(infra.AdmissionServiceName, infra.IstioNamespace)
	if err != nil {
		return err
	}
	data := map[string]string{
		"webhookName": "pilot-webhook",
		"caCert":      base64.StdEncoding.EncodeToString(caCert),
		"serverCert":  base64.StdEncoding.EncodeToString(serverCert),
		"serverKey":   base64.StdEncoding.EncodeToString(serverKey),
	}
	yaml, err := fill("pilot-webhook-secret.yaml.tmpl", data)
	if err != nil {
		return err
	}
	return infra.kubeApply(yaml, infra.IstioNamespace)
}

func (infra *infra) deleteAdmissionWebhookSecret() error {
	return util.Run(fmt.Sprintf("kubectl delete --kubeconfig %s -n %s secret pilot-webhook",
		kubeconfig, infra.IstioNamespace))
}
