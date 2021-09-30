package main

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/pkg/errors"
	admissionregistration "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	configclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	utilyaml "sigs.k8s.io/cluster-api/util/yaml"
	"sigs.k8s.io/yaml"
)

type provider struct {
	name       string
	version    string
	ptype      clusterctlv1.ProviderType
	components repository.Components
	metadata   []byte
}

var (
	providers = []provider{
		{name: "cluster-api", version: "v0.4.3", ptype: clusterctlv1.CoreProviderType},
		{name: "aws", version: "v0.7.0", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "azure", version: "v0.5.2", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "metal3", version: "v0.5.0", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "gcp", version: "v0.4.0", ptype: clusterctlv1.InfrastructureProviderType},
		{name: "openstack", version: "v0.4.0", ptype: clusterctlv1.InfrastructureProviderType},
	}
	providersPath = path.Join(projDir, "assets", "providers")
)

func (p *provider) loadComponents() error {
	configClient, err := configclient.New("")
	if err != nil {
		return err
	}

	providerConfig, err := configClient.Providers().Get(p.name, p.ptype)
	if err != nil {
		return err
	}

	repo, err := repository.NewGitHubRepository(providerConfig, configClient.Variables())
	if err != nil {
		return err
	}

	p.metadata, err = repo.GetFile(p.version, "metadata.yaml")
	if err != nil {
		return err
	}

	options := repository.ComponentsOptions{
		TargetNamespace:     "openshift-cluster-api",
		SkipTemplateProcess: true,
		Version:             p.version,
	}

	componentsFile, err := repo.GetFile(options.Version, repo.ComponentsPath())
	if err != nil {
		return errors.Wrapf(err, "failed to read %q from provider's repository %q", repo.ComponentsPath(), providerConfig.ManifestLabel())
	}

	ci := repository.ComponentsInput{
		Provider:     providerConfig,
		ConfigClient: configClient,
		Processor:    yamlprocessor.NewSimpleProcessor(),
		RawYaml:      componentsFile,
		Options:      options}

	p.components, err = repository.NewComponents(ci)
	return err
}

func (p *provider) writeProvider(objs []unstructured.Unstructured) error {
	combined, err := utilyaml.FromUnstructured(objs)
	if err != nil {
		return err
	}
	providerTypeName := strings.ReplaceAll(strings.ToLower(string(p.ptype)), "provider", "")
	fNameBase := providerTypeName + "-" + p.name

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.version,
			Namespace: "openshift-cluster-api",
			Labels: map[string]string{
				"provider-name": p.name,
				"provider-type": providerTypeName,
			},
		},
		Data: map[string]string{
			"metadata":   string(p.metadata),
			"components": string(combined),
		},
	}

	cmYaml, err := yaml.Marshal(cm)
	if err != nil {
		return err
	}

	fName := strings.ToLower(fNameBase + ".yaml")
	return os.WriteFile(path.Join(providersPath, fName), cmYaml, 0600)
}

func (p *provider) findWebhookCertSecretName() map[string]string {
	certSecretNames := map[string]string{}

	for _, obj := range p.components.Objs() {
		switch obj.GetKind() {
		case "CustomResourceDefinition":
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := scheme.Convert(&obj, crd, nil); err != nil {
				panic(err)
			}
			if sec, ok := crd.Annotations["cert-manager.io/inject-ca-from"]; ok {
				certSecretNames[crd.Spec.Conversion.Webhook.ClientConfig.Service.Name] = strings.Split(sec, "/")[1]
			}

		case "MutatingWebhookConfiguration":
			mwc := &admissionregistration.MutatingWebhookConfiguration{}
			if err := scheme.Convert(&obj, mwc, nil); err != nil {
				panic(err)
			}
			if sec, ok := mwc.Annotations["cert-manager.io/inject-ca-from"]; ok {
				certSecretNames[mwc.Webhooks[0].ClientConfig.Service.Name] = strings.Split(sec, "/")[1]
			}

		case "ValidatingWebhookConfiguration":
			vwc := &admissionregistration.ValidatingWebhookConfiguration{}
			if err := scheme.Convert(&obj, vwc, nil); err != nil {
				panic(err)
			}
			if sec, ok := vwc.Annotations["cert-manager.io/inject-ca-from"]; ok {
				certSecretNames[vwc.Webhooks[0].ClientConfig.Service.Name] = strings.Split(sec, "/")[1]
			}
		}
	}
	return certSecretNames
}

func importProviders() error {
	for _, p := range providers {
		err := p.loadComponents()
		if err != nil {
			return err
		}
		fmt.Println(p.ptype, p.name)
		certSecretNames := p.findWebhookCertSecretName()

		finalObjs := []unstructured.Unstructured{}
		for _, obj := range p.components.Objs() {
			switch obj.GetKind() {
			case "CustomResourceDefinition", "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
				anns := obj.GetAnnotations()
				if anns == nil {
					anns = map[string]string{}
				}
				if _, ok := anns["cert-manager.io/inject-ca-from"]; ok {
					anns["service.beta.openshift.io/inject-cabundle"] = "true"
					delete(anns, "cert-manager.io/inject-ca-from")
					obj.SetAnnotations(anns)
				}
				finalObjs = append(finalObjs, obj)
			case "Service":
				anns := obj.GetAnnotations()
				if anns == nil {
					anns = map[string]string{}
				}
				if name, ok := certSecretNames[obj.GetName()]; ok {
					anns["service.beta.openshift.io/serving-cert-secret-name"] = name
					obj.SetAnnotations(anns)
				}
				finalObjs = append(finalObjs, obj)
			case "Certificate", "Issuer", "Namespace": // skip
			case "Deployment":
				// TODO replace the images with openshift built ones..
				finalObjs = append(finalObjs, obj)
			default:
				finalObjs = append(finalObjs, obj)
			}
		}

		err = p.writeProvider(finalObjs)
		if err != nil {
			return err
		}
	}
	return nil
}