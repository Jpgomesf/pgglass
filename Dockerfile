FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pgglass /usr/local/bin/pgglass
ENV XDG_CACHE_HOME=/tmp
WORKDIR /etc/pgglass
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pgglass"]
