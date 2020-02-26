FROM golang:1.14 as build
WORKDIR /src/app
COPY go.mod go.sum ./
RUN go mod download # for faster development
COPY . .
RUN go build -v .

FROM debian:buster-slim
RUN apt-get update && apt-get install -y git && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/app/reposync /bin/reposync
CMD ["/bin/reposync"]