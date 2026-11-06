FROM golang:1.26 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o proxmox-cloud-controller-manager .

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /app/proxmox-cloud-controller-manager /

ENTRYPOINT ["/proxmox-cloud-controller-manager"]
