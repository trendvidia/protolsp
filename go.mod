module github.com/trendvidia/protolsp

go 1.26.2

require (
	buf.build/go/protovalidate v1.2.0
	connectrpc.com/connect v1.20.0
	github.com/coder/websocket v1.8.14
	github.com/google/cel-go v0.28.0
	github.com/stretchr/testify v1.12.1
	github.com/trendvidia/govm/v2 v2.11.0
	github.com/trendvidia/protocheck-govm v0.2.0
	github.com/trendvidia/protocheck/v2 v2.7.0
	github.com/trendvidia/protocompile v0.35.1
	github.com/trendvidia/protoregistry v0.72.0
	github.com/trendvidia/protowire-go v1.12.0
	go.lsp.dev/jsonrpc2 v0.10.0
	go.lsp.dev/protocol v0.12.0
	go.lsp.dev/uri v0.3.0
	golang.org/x/net v0.58.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/trendvidia/protowire v1.13.0

require (
	buf.build/gen/go/bufbuild/protodescriptor/protocolbuffers/go v1.36.12-20261012172039-ff528c23443f.2 // indirect
	// Pinned above what MVS would pick: these generated-SDK versions sort
	// by protobuf-go prefix first, so protocompile's older
	// v1.36.12-20240920… would win over v1.36.11-20260415… and drop the
	// types protovalidate v1.2.0 needs (FieldPath, Rule, FieldRules).
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260415201107-50325440f8f2.1 // indirect
	cel.dev/expr v0.25.2 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/awnumar/memcall v0.4.0 // indirect
	github.com/awnumar/memguard v0.23.0 // indirect
	github.com/petermattis/goid v0.0.0-20260113132338-7c7de50cc741 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.3.4 // indirect
	github.com/tidwall/btree v1.8.2 // indirect
	go.lsp.dev/pkg v0.0.0-20210717090340-384b27a52fb2 // indirect
	go.uber.org/atomic v1.9.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.21.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/exp v0.0.0-20250911091902-df9299821621 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260807164820-c8921c73eeea // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260729162451-8efbd57d26e0 // indirect
)

// protoregistry/client depends on namespace-aware registry types
// (NamespacedFiles/NamespacedTypes) that live in the TrendVidia fork
// of protobuf-go, not upstream. Replace directives don't transit, so
// this module declares the same pin.
replace google.golang.org/protobuf => github.com/trendvidia/protobuf-go v1.36.12
