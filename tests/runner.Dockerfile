FROM alpine:3.22
RUN apk add --no-cache docker-cli bind-tools curl
COPY tests/runner-entry.sh /usr/local/bin/mosdns-test-runner
RUN chmod 0555 /usr/local/bin/mosdns-test-runner
ENTRYPOINT ["/usr/local/bin/mosdns-test-runner"]
