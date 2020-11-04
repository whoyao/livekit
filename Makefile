
# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif


build: server client

client: generate
	@{ \
  	echo "building client" ;\
  	mkdir -p bin ;\
  	cd cmd/cli ;\
  	go build -i -o ../../bin/livekit-cli ;\
  	}

server: generate
	@{ \
  	echo "building server" ;\
  	mkdir -p bin ;\
  	cd cmd/server ;\
	go build -i -o ../../bin/livekit-server ;\
	}

generate: wire
	@{ \
  	echo "wiring" ;\
  	cd cmd/server ;\
  	$(WIRE) ;\
	}

GO_TARGET=proto/livekit
proto: protoc protoc-gen-go twirp-gen
	@{ \
  	mkdir -p $(GO_TARGET) ;\
	protoc --go_out=$(GO_TARGET) --twirp_out=$(GO_TARGET) \
    	--go_opt=paths=source_relative \
    	--twirp_opt=paths=source_relative \
    	--plugin=$(PROTOC_GEN_GO) \
    	-I=proto \
    	proto/room.proto proto/model.proto ;\
    protoc --go_out=$(GO_TARGET) \
    	--go_opt=paths=source_relative \
    	-I=proto \
    	proto/rtc.proto ;\
    }

protoc:
ifeq (, $(shell which protoc))
	echo "protoc is required, and is not installed"
endif

protoc-gen-go:
ifeq (, $(shell which protoc-gen-go))
	@{ \
	echo "installing go protobuf plugin" ;\
	go install google.golang.org/protobuf/cmd/protoc-gen-go ;\
	}
endif

twirp-gen:
ifeq (, $(shell which protoc-gen-twirp))
	@{ \
	echo "installing twirp protobuf plugin" ;\
	go install github.com/twitchtv/twirp/protoc-gen-twirp ;\
	}
endif

wire:
ifeq (, $(shell which wire))
	@{ \
	echo "installing wire" ;\
	go install github.com/google/wire/cmd/wire ;\
	}
WIRE=$(GOBIN)/wire
else
WIRE=$(shell which wire)
endif
