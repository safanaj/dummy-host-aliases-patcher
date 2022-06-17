FROM gcr.io/distroless/static
ARG component
ADD $component /

CMD [/$component]
