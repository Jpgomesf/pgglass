FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pgglass /usr/local/bin/pgglass
ENV XDG_CACHE_HOME=/tmp
WORKDIR /etc/pgglass
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pgglass"]
