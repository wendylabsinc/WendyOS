FROM scratch
ARG APP_VERSION
ARG SOURCE_DATE_EPOCH=0
LABEL org.opencontainers.image.version=$APP_VERSION
WORKDIR /app
COPY server /app/server
COPY version.txt /app/version.txt
ENTRYPOINT ["/app/server"]
