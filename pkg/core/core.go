package core

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
	kscorev1alpha1 "kubesphere.io/api/core/v1alpha1"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	restconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/hashicorp/go-version"
	"github.com/kubesphere-extensions/upgrade/pkg/config"
	"github.com/kubesphere-extensions/upgrade/pkg/hooks"
	_ "github.com/kubesphere-extensions/upgrade/pkg/hooks/whizard-monitoring"
)

type CoreHelper struct {
	extensionName    string
	extensionVersion string
	isExtension      bool
	cfg              *config.ExtensionUpgradeHookConfig
	chart            *chart.Chart

	client        runtimeclient.Client
	scheme        *runtime.Scheme
	dynamicClient *dynamic.DynamicClient
}

func NewCoreHelper() (*CoreHelper, error) {
	restConfig, err := restconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get rest config: %s", err)
	}

	scheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)
	_ = kscorev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	client, err := runtimeclient.New(restConfig, runtimeclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %s", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %s", err)
	}

	extensionName := config.GetHookEnvReleaseName()
	isExtension := true
	if strings.HasSuffix(config.GetHookEnvReleaseName(), "-agent") {
		extensionName = strings.TrimSuffix(config.GetHookEnvReleaseName(), "-agent")
		isExtension = false
	}
	c := &CoreHelper{
		extensionName: extensionName,
		isExtension:   isExtension,
		dynamicClient: dynamicClient,
		client:        client,
		scheme:        scheme,
	}

	chart, err := loadChart(config.GetHookEnvChartPath(), "values.yaml")
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %s", err)
	}
	cfg, err := config.LoadConfigFromHelmValues(chart.Values)
	if err != nil {
		klog.Errorf("failed to load config from helm values: %s", err)

		klog.Info("try to use default config")
		if defaultCfg, ok := config.NewConfig().ExtensionUpgradeHookConfigs[extensionName]; ok {
			cfg = &defaultCfg
		}
	}

	c.chart = chart
	c.extensionVersion = chart.Metadata.Version

	c.cfg = cfg
	klog.Infof("extension %s upgrade config: %+v", c.extensionName, cfg)

	return c, nil
}

func (c *CoreHelper) Run(ctx context.Context) error {

	if c.cfg == nil || !c.cfg.Enabled {
		klog.Info("config not found, skip extension upgrade")
		return nil
	}

	err := func(ctx context.Context) error {
		if err := c.runApplyCRDs(ctx); err != nil {
			return fmt.Errorf("failed to apply CRDs: %s", err)
		}
		if err := c.runBuiltInHooks(ctx); err != nil {
			return fmt.Errorf("failed to run built-in hooks: %s", err)
		}
		if err := c.runExtraConfig(ctx); err != nil {
			return fmt.Errorf("failed to run extra config: %s", err)
		}
		return nil
	}(ctx)
	if err != nil {
		if c.cfg.FailurePolicy == config.IgnoreError {
			klog.Errorf("extension %s upgrade failed, but ignoring error due to failure policy: %s", c.extensionName, err)
			return nil
		}
		return err
	}

	klog.Infof("extension %s upgrade completed successfully", c.extensionName)

	return nil
}

func (c *CoreHelper) runApplyCRDs(ctx context.Context) error {
	if config.GetHookEnvAction() == config.ActionInstall && c.cfg.InstallCrds ||
		config.GetHookEnvAction() == config.ActionUpgrade && c.cfg.UpgradeCrds {

		klog.Info("force update of crd before extension installation or upgrade")

		if c.isExtension {
			return c.applyCRDsFromSubchartsByTag(ctx, "extension")
		} else {
			return c.applyCRDsFromSubchartsByTag(ctx, "agent")
		}
	}
	return nil
}

func (c *CoreHelper) runBuiltInHooks(ctx context.Context) error {
	if hook, ok := hooks.GetHook(c.extensionName); ok {
		klog.Infof("running hook: %s\n", c.extensionName)
		if err := hook.Run(ctx, c.client, c.cfg); err != nil {
			return fmt.Errorf("failed to run hook: %s", err)
		}
	}
	return nil
}

func (c *CoreHelper) runExtraConfig(ctx context.Context) error {

	if len(c.cfg.ExtraConfig) == 0 {
		klog.Info("no extra config found, skip extension upgrade extra")
		return nil
	}

	extensionVersion := version.Must(version.NewVersion(c.extensionVersion))

	for _, extraConfig := range c.cfg.ExtraConfig {
		klog.Info("extra config: ", extraConfig)
		if extraConfig.Action != "" && extraConfig.Action != config.GetHookEnvAction() {
			klog.Infof("skip extra config %s, action not match: %s", extraConfig.VersionConstraint, extraConfig.Action)
			continue
		}

		if extraConfig.Scene != "" && extraConfig.Scene == "extension" && !c.isExtension ||
			extraConfig.Scene != "" && extraConfig.Scene == "agent" && c.isExtension {
			klog.Infof("skip extra config %s, scene not match: %s", extraConfig.VersionConstraint, extraConfig.Scene)
			continue
		}

		constraints, err := version.NewConstraint(extraConfig.VersionConstraint)
		if err != nil {
			klog.Errorf("failed to parse version constraint: %s, error: %v", extraConfig.VersionConstraint, err)
			continue
		}
		if !constraints.Check(extensionVersion) {
			klog.Infof("skip extra config %s, version not match: %s", extraConfig.VersionConstraint, extensionVersion)
			continue
		}

		if len(extraConfig.Command) != 0 {
			command := exec.CommandContext(ctx, "bash", "-c", strings.Join(extraConfig.Command, " "))
			output, err := command.CombinedOutput()

			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("command execution timed out: %s", extraConfig.Command)
			}

			if err != nil {
				return fmt.Errorf("failed to execute command: %s, error: %v", extraConfig.Command, err)
			}
			klog.Infof("command executed successfully: %s, output: %s", extraConfig.Command, output)
		}
	}

	return nil
}
