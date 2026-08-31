# Accounts microservice.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /auth .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /auth /app/auth
# USERS_DIR is retained for local fallback and one-time JSON import; production accounts use DB.
ENV PORT=8099 USERS_DIR=/data/auth
EXPOSE 8099
ENTRYPOINT ["/app/auth"]
