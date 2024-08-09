/*
Copyright 2024 The Crossplane Authors.

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

package validate

import (
	"fmt"
	"io"

	"github.com/spf13/afero"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/fieldpath"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	metav1 "github.com/crossplane/crossplane/apis/pkg/meta/v1"
	metav1beta1 "github.com/crossplane/crossplane/apis/pkg/meta/v1beta1"
	"github.com/crossplane/crossplane/internal/xcrd"
)

const (
	packageFileName = "package.yaml"
	baseLayerLabel  = "base"

	refFmt   = "%s@%s"
	imageFmt = "%s:%s"
)

// Manager defines a Manager for preparing Crossplane packages for validation.
type Manager struct {
	fetcher ImageFetcher
	cache   Cache
	writer  io.Writer

	crds []*extv1.CustomResourceDefinition
	deps map[string]interface{} // Dependency images (providers, configurations, functions)
}

// NewManager returns a new Manager.
func NewManager(cacheDir string, fs afero.Fs, w io.Writer) *Manager {
	m := &Manager{}

	m.cache = &LocalCache{
		fs:       fs,
		cacheDir: cacheDir,
	}

	m.fetcher = &Fetcher{}
	m.writer = w
	m.crds = make([]*extv1.CustomResourceDefinition, 0)
	m.deps = make(map[string]interface{})

	return m
}

// PrepExtensions converts the unstructured XRDs/CRDs to CRDs and extract package images to add as a dependency.
func (m *Manager) PrepExtensions(extensions []*unstructured.Unstructured) error { //nolint:gocognit // the function itself is not that complex, it just has different cases
	for _, e := range extensions {
		switch e.GroupVersionKind().GroupKind() {
		case schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:
			crd := &extv1.CustomResourceDefinition{}
			bytes, err := e.MarshalJSON()
			if err != nil {
				return errors.Wrap(err, "cannot marshal CRD to JSON")
			}

			if err := yaml.Unmarshal(bytes, crd); err != nil {
				return errors.Wrap(err, "cannot unmarshal CRD YAML")
			}

			m.crds = append(m.crds, crd)

		case schema.GroupKind{Group: "apiextensions.crossplane.io", Kind: "CompositeResourceDefinition"}:
			xrd := &v1.CompositeResourceDefinition{}
			bytes, err := e.MarshalJSON()
			if err != nil {
				return errors.Wrap(err, "cannot marshal XRD to JSON")
			}

			if err := yaml.Unmarshal(bytes, xrd); err != nil {
				return errors.Wrap(err, "cannot unmarshal XRD YAML")
			}

			crd, err := xcrd.ForCompositeResource(xrd)
			if err != nil {
				return errors.Wrapf(err, "cannot derive composite CRD from XRD %q", xrd.GetName())
			}
			m.crds = append(m.crds, crd)

			if xrd.Spec.ClaimNames != nil {
				claimCrd, err := xcrd.ForCompositeResourceClaim(xrd)
				if err != nil {
					return errors.Wrapf(err, "cannot derive claim CRD from XRD %q", xrd.GetName())
				}

				m.crds = append(m.crds, claimCrd)
			}

		case schema.GroupKind{Group: "pkg.crossplane.io", Kind: "Provider"}:
			paved := fieldpath.Pave(e.Object)
			image, err := paved.GetString("spec.package")
			if err != nil {
				return errors.Wrapf(err, "cannot get package image")
			}

			m.deps[image] = nil

		case schema.GroupKind{Group: "pkg.crossplane.io", Kind: "Configuration"}:
			paved := fieldpath.Pave(e.Object)
			image, err := paved.GetString("spec.package")
			if err != nil {
				return errors.Wrapf(err, "cannot get package image")
			}

			m.deps[image] = nil

		case schema.GroupKind{Group: "meta.pkg.crossplane.io", Kind: "Configuration"}:
			meta, err := e.MarshalJSON()
			if err != nil {
				return errors.Wrap(err, "cannot marshal configuration to JSON")
			}

			cfg := &metav1.Configuration{}
			if err := yaml.Unmarshal(meta, cfg); err != nil {
				return errors.Wrapf(err, "cannot unmarshal configuration YAML")
			}

			m.deps[cfg.Name] = cfg

		case schema.GroupKind{Group: "meta.pkg.crossplane.io", Kind: "Provider"}:
			meta, err := e.MarshalJSON()
			if err != nil {
				return errors.Wrap(err, "cannot marshal provider to JSON")
			}
			p := &metav1.Configuration{}
			if err := yaml.Unmarshal(meta, p); err != nil {
				return errors.Wrapf(err, "cannot unmarshal configuration YAML")
			}

			m.deps[p.Name] = p

		case schema.GroupKind{Group: "meta.pkg.crossplane.io", Kind: "Function"}:
			meta, err := e.MarshalJSON()
			if err != nil {
				return errors.Wrap(err, "cannot marshal function to JSON")
			}

			f := &metav1beta1.Function{}
			if err := yaml.Unmarshal(meta, f); err != nil {
				return errors.Wrapf(err, "cannot unmarshal function YAML")
			}

			m.deps[f.Name] = f

		default:
			continue
		}
	}

	return nil
}

// CacheAndLoad finds and caches dependencies and loads them as CRDs.
func (m *Manager) CacheAndLoad(cleanCache bool) error {
	if cleanCache {
		if err := m.cache.Flush(); err != nil {
			return errors.Wrapf(err, "cannot flush cache directory")
		}
	}

	if err := m.cache.Init(); err != nil {
		return errors.Wrapf(err, "cannot initialize cache directory")
	}

	if err := m.addDependencies(m.deps); err != nil {
		return errors.Wrapf(err, "cannot add package dependencies")
	}

	if err := m.cacheDependencies(); err != nil {
		return errors.Wrapf(err, "cannot cache package dependencies")
	}

	schemas, err := m.cache.Load()
	if err != nil {
		return errors.Wrapf(err, "cannot load cache")
	}

	return m.PrepExtensions(schemas)
}

func (m *Manager) addDependencies(deps map[string]interface{}) error {
	if len(deps) == 0 {
		return nil
	}

	deepDeps := make(map[string]interface{})
	for image := range deps {
		pkg := m.deps[image]

		if pkg == nil {
			var err error
			pkg, err = m.downloadAndExtractPackageWithType(image)
			if err != nil {
				return err
			}
			m.deps[image] = pkg
		}

		dependsOn, err := getDependencies(pkg)
		if err != nil {
			return err
		}

		for _, dep := range dependsOn {
			image := ""
			if dep.Configuration != nil { //nolint:gocritic // switch is not suitable here
				image = *dep.Configuration
			} else if dep.Provider != nil {
				image = *dep.Provider
			} else if dep.Function != nil {
				image = *dep.Function
			}
			if len(image) > 0 {
				image = fmt.Sprintf(imageFmt, image, dep.Version)

				if _, ok := m.deps[image]; !ok {
					deepDeps[image] = nil
					m.deps[image] = nil
				}
			}
		}
	}

	return m.addDependencies(deepDeps)
}

func (m *Manager) cacheDependencies() error {
	if err := m.cache.Init(); err != nil {
		return errors.Wrapf(err, "cannot initialize  cache directory")
	}

	for image := range m.deps {
		path, err := m.cache.Exists(image) // returns the path if the image is not cached
		if err != nil {
			return errors.Wrapf(err, "cannot check if cache exists for %s", image)
		}

		if path == "" {
			continue
		}

		if _, err := fmt.Fprintln(m.writer, "package schemas does not exist, downloading: ", image); err != nil {
			return errors.Wrapf(err, errWriteOutput)
		}

		layer, err := m.fetcher.FetchBaseLayer(image)
		if err != nil {
			return errors.Wrapf(err, "cannot download package %s", image)
		}

		schemas, _, err := extractPackageContent(*layer)
		if err != nil {
			return errors.Wrapf(err, "cannot extract package file and meta")
		}

		if err := m.cache.Store(schemas, path); err != nil {
			return errors.Wrapf(err, "cannot store base layer")
		}
	}

	return nil
}

func getDependencies(pkg interface{}) ([]metav1.Dependency, error) {
	switch v := pkg.(type) {
	case *metav1.Configuration:
		return v.GetDependencies(), nil
	case *metav1.Provider:
		return v.GetDependencies(), nil
	case *metav1beta1.Function:
		return v.GetDependencies(), nil
	default:
		return nil, errors.New("unknown package type")
	}
}

func findPackageYamlType(meta []byte) (interface{}, error) {
	// Define a list of possible types
	candidates := []interface{}{
		&metav1.Configuration{},
		&metav1.Provider{},
		&metav1beta1.Function{},
	}

	// Try to unmarshal into each type
	for _, candidate := range candidates {
		if err := yaml.Unmarshal(meta, candidate); err == nil {
			// If successful, return the candidate
			return candidate, nil
		}
	}

	return nil, errors.New("cannot unmarshal dependency YAML")
}

func (m *Manager) downloadAndExtractPackageWithType(image string) (interface{}, error) {
	layer, err := m.fetcher.FetchBaseLayer(image)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot download package %s", image)
	}

	_, meta, err := extractPackageContent(*layer)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot extract package file and meta")
	}
	pkg, err := findPackageYamlType(meta)
	if err != nil {
		return nil, err
	}
	return pkg, nil
}
