install-lnd:
	@go install ./cmd/psweb
	@echo "psweb installed to $$(go env GOPATH)/bin/"

install-cln:
	@go install -tags cln ./cmd/psweb
	@echo "psweb installed to $$(go env GOPATH)/bin/"
	@echo "Add 'plugin=$$(go env GOPATH)/bin/psweb' to $${HOME}/.lightning/config"