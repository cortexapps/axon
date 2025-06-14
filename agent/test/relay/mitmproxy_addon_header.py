"""Add an HTTP header to each response."""


class AddHeader:
    def response(self, flow):
        flow.response.headers["x-proxy-mitmproxy"] = "true"


addons = [AddHeader()]