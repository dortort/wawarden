FROM scratch AS data
WORKDIR /data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
ARG TARGETARCH
ARG VERSION
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/dortort/wawarden" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --chmod=0555 dist/wawarden_${VERSION}_linux_${TARGETARCH} /wawarden
COPY dist/licenses /licenses
# An empty named volume mounted on /data takes this directory's owner and mode.
COPY --from=data --chown=65532:65532 --chmod=0700 /data /data
USER 65532:65532
ENTRYPOINT ["/wawarden"]
CMD ["serve"]
