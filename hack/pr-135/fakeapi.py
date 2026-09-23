"""Stub API server for PR 135 startup tests: fakeapi.py <port>

Plain HTTP. Serves discovery for config.openshift.io/v1 with the APIServer
kind; MODE selects the failure:

  503       503 on GET apiservers/cluster
  hang      GET apiservers/cluster never answers
  dis503    503 on discovery of config.openshift.io/v1
  dischang  discovery of config.openshift.io/v1 never answers

Every other path returns 404. Requests are logged to stderr.
"""
import json, os, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer

MODE = os.environ.get("MODE", "503")

DISCOVERY = {
    "/api": {"kind": "APIVersions", "versions": ["v1"],
             "serverAddressByClientCIDRs": [{"clientCIDR": "0.0.0.0/0", "serverAddress": ""}]},
    "/apis": {"kind": "APIGroupList", "apiVersion": "v1", "groups": [{
        "name": "config.openshift.io",
        "versions": [{"groupVersion": "config.openshift.io/v1", "version": "v1"}],
        "preferredVersion": {"groupVersion": "config.openshift.io/v1", "version": "v1"}}]},
    "/apis/config.openshift.io/v1": {"kind": "APIResourceList", "apiVersion": "v1",
        "groupVersion": "config.openshift.io/v1", "resources": [
            {"name": "apiservers", "singularName": "apiserver", "namespaced": False,
             "kind": "APIServer", "verbs": ["get", "list", "watch"]}]},
}

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): sys.stderr.write("REQ %s\n" % self.path); sys.stderr.flush()

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/apis/config.openshift.io/v1" and MODE == "dischang":
            time.sleep(600); return
        if path == "/apis/config.openshift.io/v1" and MODE == "dis503":
            self.send_response(503); self.send_header("Content-Length", "0"); self.end_headers(); return
        if path in DISCOVERY:
            body = json.dumps(DISCOVERY[path]).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        if path.startswith("/apis/config.openshift.io/v1/apiservers"):
            if MODE == "hang":
                time.sleep(600); return
            body = json.dumps({"kind": "Status", "apiVersion": "v1", "status": "Failure",
                               "message": "the server is currently unable to handle the request",
                               "reason": "ServiceUnavailable", "code": 503}).encode()
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body); return
        self.send_response(404); self.send_header("Content-Length", "0"); self.end_headers()

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
