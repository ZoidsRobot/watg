# BASE IMAGE
FROM golang:1-alpine

# INSTALL DEPS
RUN apk --no-cache add \
                   go \
                   gcc \
                   g++ \
                   make \
                   git \
                   libwebp-dev \
                   libwebp-tools \
                   tzdata \
                   ffmpeg \
                   imagemagick

# SET WORKDIR
WORKDIR /go/src/watgbridge

# COPY ALL FILES/FOLDERS
COPY . ./

# BUILD
RUN go mod download
RUN go build

# RUN
CMD ["./watgbridge"]
