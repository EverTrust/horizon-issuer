module gitlab.com/evertrust/horizon-cm

go 1.16

replace gitlab.com/evertrust/horizon-go => /Users/antonin/Sites/horizon-go

require (
	github.com/jetstack/cert-manager v1.6.1
	github.com/onsi/ginkgo v1.16.4
	github.com/onsi/gomega v1.15.0
	gitlab.com/evertrust/horizon-go v0.0.0-00010101000000-000000000000 // indirect
	k8s.io/api v0.22.2
	k8s.io/apimachinery v0.22.2
	k8s.io/client-go v0.22.2
	sigs.k8s.io/controller-runtime v0.10.1
)
