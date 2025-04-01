FROM golang:1.23 as build
COPY . /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -ldflags '-w -s' -trimpath -o chartherd ./cmd/chartherd

FROM scratch
LABEL org.opencontainers.image.source https://github.com/evgeny-terekhov/chartherd
COPY --from=build /src/chartherd .
COPY --from=build /usr/share/ca-certificates/ /usr/share/ca-certificates/
COPY --from=build /etc/ssl/certs /etc/ssl/certs

CMD ["./chartherd"]
