# Build the context with `npm run device:package` from web-client.
FROM scratch
ARG TARGETARCH
ARG WENDY_WEB_PASSWORD
ARG WENDY_WEB_ALLOW_OPEN=false
ENV WENDY_WEB_PASSWORD=${WENDY_WEB_PASSWORD} \
    WENDY_WEB_ALLOW_OPEN=${WENDY_WEB_ALLOW_OPEN} \
    WENDY_AGENT_SOCKET=/run/wendy/agent/agent.sock
COPY wendy-device-web-${TARGETARCH} /wendy-device-web
COPY www /www
EXPOSE 8080
ENTRYPOINT ["/wendy-device-web"]
