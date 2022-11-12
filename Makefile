build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOARM=7 GOAMD64=v3 go build -tags 'osusergo netgo' -ldflags "-s -w -extldflags -static" -o wish .

compat:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOARM=6 GOAMD64=v1 go build -tags 'osusergo netgo' -ldflags "-s -w -extldflags -static" -o wish-compat .
