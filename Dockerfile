# Ship Reports — zero dependencies, so there is nothing to install or build.
FROM node:24-alpine

WORKDIR /app
COPY server.js ./
COPY public ./public

# uid 1000 in the image, same as the host user that owns the reports tree.
USER node

ENV SHIP_REPORTS_ROOT=/reports \
    SHIP_REPORTS_HOST=0.0.0.0
EXPOSE 7788

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:7788/api/state').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

CMD ["node", "server.js", "--port", "7788"]
