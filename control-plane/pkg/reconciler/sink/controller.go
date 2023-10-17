/*
 * Copyright 2020 The Knative Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sink

import (
	"context"

	"github.com/IBM/sarama"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"knative.dev/eventing/pkg/apis/feature"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	configmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap"
	podinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/pod"
	secretinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/secret"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/network"

	eventing "knative.dev/eventing-kafka-broker/control-plane/pkg/apis/eventing/v1alpha1"
	sinkinformer "knative.dev/eventing-kafka-broker/control-plane/pkg/client/injection/informers/eventing/v1alpha1/kafkasink"
	sinkreconciler "knative.dev/eventing-kafka-broker/control-plane/pkg/client/injection/reconciler/eventing/v1alpha1/kafkasink"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/config"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/prober"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/reconciler/base"
)

func NewController(ctx context.Context, watcher configmap.Watcher, configs *config.Env) *controller.Impl {

	eventing.RegisterConditionSet(base.IngressConditionSet)

	logger := logging.FromContext(ctx)

	configmapInformer := configmapinformer.Get(ctx)

	reconciler := &Reconciler{
		Reconciler: &base.Reconciler{
			KubeClient:                  kubeclient.Get(ctx),
			PodLister:                   podinformer.Get(ctx).Lister(),
			SecretLister:                secretinformer.Get(ctx).Lister(),
			DataPlaneConfigMapNamespace: configs.DataPlaneConfigMapNamespace,
			ContractConfigMapName:       configs.ContractConfigMapName,
			ContractConfigMapFormat:     configs.ContractConfigMapFormat,
			DataPlaneNamespace:          configs.SystemNamespace,
			ReceiverLabel:               base.SinkReceiverLabel,
		},
		ConfigMapLister:            configmapInformer.Lister(),
		NewKafkaClusterAdminClient: sarama.NewClusterAdmin,
		Env:                        configs,
	}

	_, err := reconciler.GetOrCreateDataPlaneConfigMap(ctx)
	if err != nil {
		logger.Fatal("Failed to get or create data plane config map",
			zap.String("configmap", configs.DataPlaneConfigMapAsString()),
			zap.Error(err),
		)
	}

	featureStore := feature.NewStore(logging.FromContext(ctx).Named("feature-config-store"))
	featureStore.WatchConfigs(watcher)

	features := feature.FromContext(ctx)
	caCerts, err := reconciler.getCaCerts()
	if err != nil && (features.IsStrictTransportEncryption() || features.IsPermissiveTransportEncryption()) {
		logger.Warn("failed to get CA certs when at least one address uses TLS", zap.Error(err))
	}
	impl := sinkreconciler.NewImpl(ctx, reconciler, func(impl *controller.Impl) controller.Options {
		return controller.Options{
			ConfigStore: featureStore}
	})
	IPsLister := prober.IPsListerFromService(types.NamespacedName{Namespace: configs.SystemNamespace, Name: configs.IngressName})
	reconciler.IngressHost = network.GetServiceHostname(configs.IngressName, configs.SystemNamespace)
	reconciler.Prober, err = prober.NewComposite(ctx, configs.IngressPodPort, configs.IngressPodTlsPort, IPsLister, impl.EnqueueKey, &caCerts)
	if err != nil {
		logger.Fatal("Failed to create prober", zap.Error(err))
	}
	sinkInformer := sinkinformer.Get(ctx)

	sinkInformer.Informer().AddEventHandler(controller.HandleAll(impl.Enqueue))

	globalResync := func(_ interface{}) {
		impl.GlobalResync(sinkInformer.Informer())
	}

	configmapInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.FilterWithNameAndNamespace(configs.DataPlaneConfigMapNamespace, configs.ContractConfigMapName),
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				globalResync(obj)
			},
			DeleteFunc: func(obj interface{}) {
				globalResync(obj)
			},
		},
	})

	reconciler.Tracker = impl.Tracker

	rotateCACerts := func(obj interface{}) {
		newCerts, err := reconciler.getCaCerts()
		if err != nil && (features.IsPermissiveTransportEncryption() || features.IsStrictTransportEncryption()) {
			// We only need to warn here as the broker won't reconcile properly without the proper certs because the prober won't succeed
			logger.Warn("Failed to get new CA certs while rotating CA certs when at least one address uses TLS", zap.Error(err))
		}
		reconciler.Prober.RotateRootCaCerts(&newCerts)
		globalResync(obj)
	}

	secretinformer.Get(ctx).Informer().AddEventHandler(controller.HandleAll(
		// Call the tracker's OnChanged method, but we've seen the objects
		// coming through this path missing TypeMeta, so ensure it is properly
		// populated.
		controller.EnsureTypeMeta(
			reconciler.Tracker.OnChanged,
			corev1.SchemeGroupVersion.WithKind("Secret"),
		),
	))

	sinkInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: reconciler.OnDeleteObserver,
	})

	secretinformer.Get(ctx).Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.FilterWithName(sinkIngressTLSSecretName),
		Handler:    controller.HandleAll(rotateCACerts),
	})

	return impl
}
