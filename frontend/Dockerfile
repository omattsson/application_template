# syntax=docker/dockerfile:1
# Build stage. It runs on the build machine's platform ($BUILDPLATFORM): the
# built dist/ files do not depend on the CPU architecture, so a multi-platform
# build compiles the app once and natively, not under QEMU emulation.
FROM --platform=$BUILDPLATFORM node:26-alpine AS build

WORKDIR /app

# Copy package files (.npmrc sets legacy-peer-deps, which npm ci needs)
COPY package*.json ./
COPY .npmrc* ./

# Install dependencies (the npm cache mount speeds up repeated builds)
RUN --mount=type=cache,target=/root/.npm \
    npm ci

# Copy source code
COPY . .

# ARG for build/dev mode
ARG NODE_ENV=production

# If production, build the app, otherwise use development mode
RUN if [ "$NODE_ENV" = "production" ]; then \
        npm run build; \
    fi

# For development, use the node image to run the dev server. The source and
# node_modules are bind/volume mounted by docker-compose.yml, so this stage
# keeps the image's default user.
FROM build AS development
EXPOSE 3000
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0"]

# For production (default), use nginx-unprivileged to serve the built app. It
# runs as uid 101 (not root) and listens on 8080: a non-root process cannot
# bind a port below 1024.
FROM nginxinc/nginx-unprivileged:alpine AS production
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
USER 101
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
