FROM golang:alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /server ./services/echo/main.go

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /server .
CMD ["./server"]
