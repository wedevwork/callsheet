# M3/M4 container acceptance image (design m3-m4-container-e2e). The build
# context is a temporary directory of explicit artifacts assembled by
# go run ./cmd/devcheck container-e2e: the static callsheet, fake-adapter
# and containeracceptance-tagged test binaries and the fixture bundle (role
# manuals and the milestone documents). No repository, home directory,
# credential or vendor executable enters the image, and nothing is pulled
# or installed: it is built with --network none from scratch.
FROM scratch
COPY callsheet /callsheet
COPY fake-adapter /fake-adapter
COPY acceptance.test /acceptance.test
COPY fixtures /fixtures
WORKDIR /tmp
USER 65532:65532
ENTRYPOINT ["/acceptance.test"]
