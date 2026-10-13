package prow

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	prowapiv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
)

type fakeProwConfigLoader struct {
	cfg *config.Config
}

func (f fakeProwConfigLoader) Config() *config.Config {
	return f.cfg
}

func launchPeriodicConfig() *config.Periodic {
	return &config.Periodic{
		JobBase: config.JobBase{
			Name:   "periodic-launch",
			Labels: map[string]string{"job-type": "launch"},
			UtilityConfig: config.UtilityConfig{
				DecorationConfig: &prowapiv1.DecorationConfig{},
			},
			Spec: &corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "test",
					Env:  []corev1.EnvVar{{Name: "RELEASE_IMAGE_LATEST", Value: "original"}},
				}},
			},
		},
	}
}

// The specs returned by these functions share structure with the live prow
// config, so setting SkipCloning on them must not write through to it: the
// config is read concurrently by other lookups and replaced by reloads.
func TestJobSpecsDoNotMutateProwConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(loader ProwConfigLoader) (*prowapiv1.ProwJob, error)
	}{
		{
			name: "JobForConfig",
			call: func(loader ProwConfigLoader) (*prowapiv1.ProwJob, error) {
				return JobForConfig(loader, "periodic-launch")
			},
		},
		{
			name: "JobForLabels",
			call: func(loader ProwConfigLoader) (*prowapiv1.ProwJob, error) {
				return JobForLabels(loader, labels.SelectorFromSet(labels.Set{"job-type": "launch"}))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			periodic := launchPeriodicConfig()
			loader := fakeProwConfigLoader{cfg: &config.Config{JobConfig: config.JobConfig{Periodics: []config.Periodic{*periodic}}}}

			pj, err := tc.call(loader)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pj.Spec.DecorationConfig == nil || pj.Spec.DecorationConfig.SkipCloning == nil || !*pj.Spec.DecorationConfig.SkipCloning {
				t.Errorf("expected the returned job spec to have SkipCloning set to true")
			}
			if periodic.DecorationConfig.SkipCloning != nil {
				t.Errorf("expected the source config's DecorationConfig to be unmodified, got SkipCloning=%v", *periodic.DecorationConfig.SkipCloning)
			}
		})
	}
}

// JobForConfig deep-copies the spec so that callers can customize the
// returned job without writing through to the live prow config.
func TestJobForConfigSpecIsIndependent(t *testing.T) {
	periodic := launchPeriodicConfig()
	loader := fakeProwConfigLoader{cfg: &config.Config{JobConfig: config.JobConfig{Periodics: []config.Periodic{*periodic}}}}

	pj, err := JobForConfig(loader, "periodic-launch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	OverrideJobEnvironment(&pj.Spec, "latest", "initial", "4.19", "test-namespace", nil)
	if got := pj.Spec.PodSpec.Containers[0].Env[0].Value; got != "latest" {
		t.Errorf("expected the returned job's env var to be overridden, got %q", got)
	}
	if got := periodic.Spec.Containers[0].Env[0].Value; got != "original" {
		t.Errorf("expected the source config's pod spec to be unmodified, got %q", got)
	}
}
