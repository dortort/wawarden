FROM scratch AS data
WORKDIR /data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
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
