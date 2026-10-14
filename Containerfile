FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
COPY operator /operator
COPY driver /driver
ENTRYPOINT ["/operator"]
