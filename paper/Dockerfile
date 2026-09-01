# Paper-trading validation service.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /paper .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /paper /app/paper
# DATA_DIR is retained for local fallback and one-time JSON import; production state uses DB.
ENV PORT=8097 DATA_DIR=/data/paper
EXPOSE 8097
ENTRYPOINT ["/app/paper"]
