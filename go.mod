module github.com/vertti/pg-tunnel

go 1.27.1

require (
	github.com/aws/aws-sdk-go-v2 v1.47.0
	github.com/aws/aws-sdk-go-v2/config v1.33.5
	github.com/aws/aws-sdk-go-v2/credentials v1.20.5
	github.com/aws/aws-sdk-go-v2/feature/rds/auth v1.7.3
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.335.0
	github.com/aws/aws-sdk-go-v2/service/rds v1.129.0
	github.com/aws/aws-sdk-go-v2/service/ssm v1.78.0
	github.com/aws/session-manager-plugin v0.0.0-20260615221425-930a08e65d3a
	github.com/gorilla/websocket v1.5.3
	github.com/jackc/pgpassfile v1.0.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/stretchr/testify v1.12.1
	github.com/twinj/uuid v0.0.0-20151029044442-89173bcdda19 // AWS plugin requires the historical UUID API.
	golang.org/x/sys v0.48.0
)

require (
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.0 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.3 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.3 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/kms v1.61.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.10.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.38.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.43.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.0 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/cihub/seelog v0.0.0-20170130134532-f561c5e57575 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/xtaci/smux v1.5.57 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sync v0.17.0 // indirect
)
