module github.com/tmax-cloud/notebook-controller-go

go 1.15

require (
	github.com/go-logr/logr v0.2.0
	github.com/golang/groupcache v0.0.0-20200121045136-8c9f03a8e57e // indirect
	github.com/kubeflow/kubeflow/components/common v0.0.0-20200812220814-d4a24161a19e
	github.com/kubeflow/kubeflow/components/notebook-controller v0.0.0-20200812220814-d4a24161a19e
	github.com/onsi/ginkgo v1.11.0
	github.com/onsi/gomega v1.7.0
	github.com/prometheus/client_golang v1.7.1
	golang.org/x/crypto v0.0.0-20200728195943-123391ffb6de // indirect
	golang.org/x/net v0.0.0-20200813134508-3edf25e44fcc // indirect
	golang.org/x/oauth2 v0.0.0-20200107190931-bf48bf16ab8d // indirect
	golang.org/x/time v0.0.0-20200630173020-3af7569d3a1e // indirect
	k8s.io/api v0.18.8
	k8s.io/apimachinery v0.18.8
	k8s.io/client-go v11.0.1-0.20190409021438-1a26190bd76a+incompatible
	k8s.io/utils v0.0.0-20200815180417-3bc9d57fc792 // indirect
	sigs.k8s.io/controller-runtime v0.6.2
)

replace (
	github.com/kubeflow/kubeflow/components/common v0.0.0-00010101000000-000000000000 => github.com/kubeflow/kubeflow/components/common v0.0.0-20200812220814-d4a24161a19e
	k8s.io/client-go v11.0.1-0.20190409021438-1a26190bd76a+incompatible => k8s.io/client-go v0.17.6
	sigs.k8s.io/controller-runtime v0.6.2 => sigs.k8s.io/controller-runtime v0.2.0
)
