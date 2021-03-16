package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/flytek8s"
	corev1 "k8s.io/api/core/v1"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/flyteorg/flytepropeller/pkg/controller/executors"
	"github.com/flyteorg/flytepropeller/pkg/signals"
	"github.com/flyteorg/flytepropeller/pkg/webhook"
	"github.com/flyteorg/flytestdlib/logger"
	"github.com/flyteorg/flytestdlib/profutils"
	"github.com/flyteorg/flytestdlib/promutils"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/flyteorg/flytepropeller/pkg/controller/config"
	"github.com/spf13/cobra"
)

var webhookCmd = &cobra.Command{
	Use:     "webhook",
	Aliases: []string{"webhooks"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebhook(context.Background(), config.GetConfig())
	},
}

func init() {
	rootCmd.AddCommand(webhookCmd)
}

func runWebhook(origContext context.Context, cfg *config.Config) error {
	// set up signals so we handle the first shutdown signal gracefully
	ctx := signals.SetupSignalHandler(origContext)

	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	fmt.Println(string(raw))

	_, kubecfg, err := getKubeConfig(ctx, cfg)
	if err != nil {
		return err
	}

	// Add the propeller subscope because the MetricsPrefix only has "flyte:" to get uniform collection of metrics.
	propellerScope := promutils.NewScope(cfg.MetricsPrefix).NewSubScope("propeller").NewSubScope(safeMetricName(cfg.LimitNamespace))

	go func() {
		err := profutils.StartProfilingServerWithDefaultHandlers(ctx, cfg.ProfilerPort.Port, nil)
		if err != nil {
			logger.Panicf(ctx, "Failed to Start profiling and metrics server. Error: %v", err)
		}
	}()

	limitNamespace := ""
	if cfg.LimitNamespace != defaultNamespace {
		limitNamespace = cfg.LimitNamespace
	}

	caBuff, err := ReadFile(filepath.Join(cfg.Webhook.CertDir, "ca.crt"))
	if err != nil {
		return err
	}

	// Creates a MutationConfig to instruct ApiServer to call this service whenever a Pod is being created.
	err = createMutationConfig(ctx, cfg, caBuff)
	if err != nil {
		return err
	}

	mgr, err := manager.New(kubecfg, manager.Options{
		Port:          cfg.Webhook.ListenPort,
		CertDir:       cfg.Webhook.CertDir,
		Namespace:     limitNamespace,
		SyncPeriod:    &cfg.DownstreamEval.Duration,
		ClientBuilder: executors.NewFallbackClientBuilder(),
	})

	if err != nil {
		logger.Fatalf(ctx, "Failed to initialize controller run-time manager. Error: %v", err)
	}

	secretsWebhook := webhook.NewSecretsWebhook(propellerScope)
	err = secretsWebhook.Register(ctx, mgr)
	if err != nil {
		logger.Fatalf(ctx, "Failed to register webhook with manager. Error: %v", err)
	}

	logger.Infof(ctx, "Starting controller-runtime manager")
	return mgr.Start(ctx)
}

func deleteMutationConfig(ctx context.Context, cfg *config.Config) error {
	kubeClient, _, err := getKubeConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to create kubeclient. Error: %w", err)
	}

	return kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, cfg.Webhook.Name, metav1.DeleteOptions{})
}

func createMutationConfig(ctx context.Context, cfg *config.Config, caCert *bytes.Buffer) error {
	kubeClient, _, err := getKubeConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to create kubeclient. Error: %w", err)
	}

	shouldAddOwnerRef := true
	podName, found := os.LookupEnv("POD_NAME")
	if !found {
		shouldAddOwnerRef = false
	}

	podNamespace, found := os.LookupEnv("POD_NAMESPACE")
	if !found {
		shouldAddOwnerRef = false
	}

	var ownerRef []metav1.OwnerReference
	webhookObjectName := cfg.Webhook.Name
	if shouldAddOwnerRef {
		// Lookup the pod to retrieve its UID
		p, err := kubeClient.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			logger.Infof(ctx, "Failed to get Pod [%v/%v]. Error: %v", podNamespace, podName, err)
			return fmt.Errorf("failed to get pod. Error: %w", err)
		}

		ownerRef = []metav1.OwnerReference{
			{
				Kind:       flytek8s.PodKind,
				Name:       p.Name,
				APIVersion: corev1.SchemeGroupVersion.Version,
				UID:        p.UID,
			},
		}

		// Use the pod's name as the object name to ensure uniqueness.
		webhookObjectName = ownerRef[0].Name
	}

	path := webhook.GetPodMutatePath()
	fail := admissionregistrationv1.Fail
	sideEffects := admissionregistrationv1.SideEffectClassNoneOnDryRun

	mutateConfig := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            webhookObjectName,
			OwnerReferences: ownerRef,
		},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: webhookName,
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: caCert.Bytes(), // CA bundle created earlier
					Service: &admissionregistrationv1.ServiceReference{
						Name: cfg.Webhook.Name,
						Path: &path,
					},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{
					{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Create,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{"*"},
							APIVersions: []string{"v1"},
							Resources:   []string{"pods"},
						},
					},
				},
				FailurePolicy: &fail,
				SideEffects:   &sideEffects,
				AdmissionReviewVersions: []string{
					"v1",
					"v1beta1",
				},
			}},
	}

	if len(cfg.Webhook.Namespace) > 0 {
		mutateConfig.Webhooks[0].ClientConfig.Service.Namespace = cfg.Webhook.Namespace

		// It looks like the client ignores this namespace option as Webhooks are installed globally...
		mutateConfig.Namespace = cfg.Webhook.Namespace
	}

	logger.Infof(ctx, "Creating obj [%v]", mutateConfig.String())

	_, err = kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(ctx, mutateConfig, metav1.CreateOptions{})
	// TODO: Check for AlreadyExists error
	if err != nil {
		logger.Infof(ctx, "Failed to create MutatingWebhookConfiguration. Will attempt to update. Error: %v", err)
		obj, getErr := kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, mutateConfig.Name, metav1.GetOptions{})
		if getErr != nil {
			logger.Infof(ctx, "Failed to get MutatingWebhookConfiguration. Error: %v", getErr)
			return err
		}

		obj.Webhooks = mutateConfig.Webhooks
		_, err = kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, obj, metav1.UpdateOptions{})
		if err == nil {
			logger.Infof(ctx, "Successfully updated existing mutating webhook config.")
		}

		return err
	}

	return nil
}