FROM golang:1.24.3 AS builder

WORKDIR /app

# For development mode
FROM golang:1.24.3 AS development
WORKDIR /app
RUN go install github.com/cosmtrek/air@v1.49.0
CMD ["air"]

# For production mode
FROM builder as production
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o main ./api/main.go

FROM gcr.io/distroless/static-debian11:nonroot AS prod-final
WORKDIR /app/
COPY --from=production /app/main .
EXPOSE 8081
USER nonroot:nonroot
CMD ["./main"]