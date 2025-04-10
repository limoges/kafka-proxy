

HAPROXY_REPO := ghcr.io/haproxytech/helm-charts/haproxy

build: haproxy.yaml cert-manager.yaml

list-tags:
	oras repo tags $(HAPROXY_REPO) | sort -V

cert-manager.yaml:
	helm template \
  	cert-manager jetstack/cert-manager \
  	--namespace cert-manager \
  	--create-namespace \
  	--version v1.17.0 \
  	--set crds.enabled=true > $@

# Long-live OCI repositories
haproxy.yaml: haproxy-values.yaml
	helm template \
		oci://$(HAPROXY_REPO) \
		--namespace haproxy \
		--version 1.24.0 > $@ \
		-f haproxy-values.yaml

apply:
	kubectl apply -f namespaces.yaml
	kubectl apply -f cert-manager.yaml
	kubectl apply -f haproxy.yaml
	kubectl apply -f kafka-proxy.yaml
