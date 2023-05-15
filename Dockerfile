#
#  ---- Build image ----
#
FROM golang AS build
WORKDIR /app
COPY . .
RUN echo $PATH
RUN make build
#
# ---- Release image ----
#
FROM scratch
# Copy our static executable
WORKDIR /usr/local/bin
COPY --from=build /app/build/bin .
ENTRYPOINT ["/usr/local/bin/broker"]
