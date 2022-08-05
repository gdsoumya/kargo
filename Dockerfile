FROM --platform=$BUILDPLATFORM brigadecore/go-tools:v0.8.0 as builder

ARG TARGETOS
ARG TARGETARCH

ARG KUSTOMIZE_VERSION=v4.5.5
RUN curl -L -o /tmp/kustomize.tar.gz \
      https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/kustomize_${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz \
    && tar xvfz /tmp/kustomize.tar.gz -C /usr/local/bin

ARG YTT_VERSION=v0.41.1
RUN curl -L -o /usr/local/bin/ytt \
      https://github.com/vmware-tanzu/carvel-ytt/releases/download/${YTT_VERSION}/ytt-linux-${TARGETARCH} \
      && chmod 755 /usr/local/bin/ytt

ARG VERSION
ARG COMMIT
ARG CGO_ENABLED=0

WORKDIR /k8sta
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY config.go .
COPY main.go .

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -o bin/k8sta \
      -ldflags "-w -X github.com/akuityio/k8sta/internal/common/version.version=${VERSION} -X github.com/akuityio/k8sta/internal/common/version.commit=${COMMIT}" \
      .

WORKDIR /k8sta/bin
RUN ln -s k8sta k8sta-controller
RUN ln -s k8sta k8sta-server

FROM alpine:3.15.4 as final

RUN apk update \
    && apk add git openssh-client \
    && addgroup -S -g 65532 nonroot \
    && adduser -S -D -u 65532 -g nonroot -G nonroot nonroot

COPY --chown=nonroot:nonroot cmd/controller/ssh_config /home/nonroot/.ssh/config
COPY --from=builder /usr/local/bin/kustomize /usr/local/bin/
COPY --from=builder /usr/local/bin/ytt /usr/local/bin/
COPY --from=builder /k8sta/bin/ /usr/local/bin/

USER nonroot

RUN git config --global credential.helper store \
    && git config --global user.name k8sta \
    && git config --global user.email k8sta@akuity.io

CMD ["/usr/local/bin/k8sta"]