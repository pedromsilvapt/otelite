BINARY=otelite
PIDFILE=$(BINARY).pid

.PHONY:  start stop test

build: otelite

$(BINARY): main.go ui.go
	go build -o $(BINARY) .
	ls -l $(BINARY)

start: otelite
	./$(BINARY) server -port 4318 -db otel.db >otel.log 2>&1 & echo $$! > $(PIDFILE)

stop:
	@if [ -f $(PIDFILE) ]; then \
		kill $$(cat $(PIDFILE)) 2>/dev/null || true; \
		rm -f $(PIDFILE); \
	fi

clean:
	rm -f $(PIDFILE) otel.db ${BINARY}

build_dist:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o $(BINARY)-linux-arm64 .
	ls -l $(BINARY)-linux-*


test:
	@curl -s http://localhost:4318/ | grep -q 'otelite' && echo "otelite root: OK" || echo "otelite root: FAILED"



.PHONY: build build_dist start stop clean