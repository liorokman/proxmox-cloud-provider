FROM golang:1.20 as builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
COPY cmd ./cmd 
COPY internal ./internal 
RUN go build -o proxmox-cloud-controller-manager .

#FROM gcr.io/distroless/base-debian12:latest
FROM debian:stable

COPY --from=builder /app/proxmox-cloud-controller-manager /

CMD ["/proxmox-cloud-controller-manager"]
