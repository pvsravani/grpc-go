module google.golang.org/grpc/security/advancedtls

go 1.22.0

require (
	github.com/google/go-cmp v0.6.0
	golang.org/x/crypto v0.32.0
	google.golang.org/grpc v1.69.2
	google.golang.org/grpc/examples v0.0.0-20201112215255-90f1b3ee835b
)

require (
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250106144421-5f5ef82da422 // indirect
	google.golang.org/protobuf v1.36.2 // indirect
)

replace google.golang.org/grpc => ../../

replace google.golang.org/grpc/examples => ../../examples
