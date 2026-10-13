FROM gcr.io/distroless/static-debian13:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pgglass /usr/local/bin/pgglass
ENV XDG_CACHE_HOME=/tmp
WORKDIR /etc/pgglass
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pgglass"]
