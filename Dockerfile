# --- Сборка ---
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webparser ./cmd/webparser \
    && mkdir -p /out/work/out

# --- Запуск: только бинарник + CA-сертификаты (HTTPS), не root ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/webparser /webparser
COPY --from=build --chown=65532:65532 /out/work /work
WORKDIR /work
ENTRYPOINT ["/webparser"]
