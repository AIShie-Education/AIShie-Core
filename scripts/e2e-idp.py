#!/usr/bin/env python3
"""A stand-in OpenID Connect provider for scripts/e2e.sh, from the standard
library alone.

    e2e-idp.py PORT

It serves two issuers on 127.0.0.1:PORT:

  /adfs  the operator's provider: its discovery document, which is all a
         server reads of a provider before anyone signs in;
  /site  a provider of the site's, which signs people in: discovery, a key
         set, an authorization endpoint that signs in IDP_SUBJECT (with
         IDP_EMAIL, vouched for) at once and sends the browser back with a
         code, and a token endpoint that redeems each code once, for the
         client IDP_CLIENT_ID with its secret IDP_CLIENT_SECRET, for an
         id_token signed RS256 with a key made at start.

It refuses a redirect URI other than IDP_REDIRECT_URI, so that the test
learns whether the server sends the one it says to register. It logs no
code, token or secret.
"""

import base64
import hashlib
import json
import os
import secrets
import sys
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def b64(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def int_bytes(n):
    return n.to_bytes((n.bit_length() + 7) // 8, "big")


# RSA, as much of it as signing takes: two probable primes by Miller-Rabin,
# and PKCS #1 v1.5 with SHA-256.
def probable_prime(n):
    if n % 2 == 0:
        return False
    for p in (3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37, 41, 43, 47):
        if n % p == 0:
            return n == p
    d, r = n - 1, 0
    while d % 2 == 0:
        d, r = d // 2, r + 1
    for _ in range(40):
        x = pow(secrets.randbelow(n - 3) + 2, d, n)
        if x in (1, n - 1):
            continue
        for _ in range(r - 1):
            x = pow(x, 2, n)
            if x == n - 1:
                break
        else:
            return False
    return True


def prime(bits):
    while True:
        c = secrets.randbits(bits) | (1 << (bits - 1)) | (1 << (bits - 2)) | 1
        if probable_prime(c):
            return c


E = 65537
while True:
    P, Q = prime(1024), prime(1024)
    PHI = (P - 1) * (Q - 1)
    if P != Q and PHI % E != 0:
        break
N, D = P * Q, pow(E, -1, PHI)
K = (N.bit_length() + 7) // 8
SHA256_INFO = bytes.fromhex("3031300d060960864801650304020105000420")


def sign(data):
    t = SHA256_INFO + hashlib.sha256(data).digest()
    em = b"\x00\x01" + b"\xff" * (K - len(t) - 3) + b"\x00" + t
    return pow(int.from_bytes(em, "big"), D, N).to_bytes(K, "big")


PORT = int(sys.argv[1])
BASE = "http://127.0.0.1:%d" % PORT
ADFS, SITE = BASE + "/adfs", BASE + "/site"
CLIENT_ID = os.environ["IDP_CLIENT_ID"]
CLIENT_SECRET = os.environ["IDP_CLIENT_SECRET"]
REDIRECT_URI = os.environ["IDP_REDIRECT_URI"]
SUBJECT, EMAIL = os.environ["IDP_SUBJECT"], os.environ["IDP_EMAIL"]
CODES, LOCK = {}, threading.Lock()


def discovery(issuer, authorize, token, keys):
    return {
        "issuer": issuer,
        "authorization_endpoint": issuer + authorize,
        "token_endpoint": issuer + token,
        "jwks_uri": issuer + keys,
        "response_types_supported": ["code"],
        "subject_types_supported": ["public"],
        "id_token_signing_alg_values_supported": ["RS256"],
        "scopes_supported": ["openid", "profile", "email"],
        "claims_supported": ["sub", "email", "email_verified", "name"],
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # nothing of a code or a token in any log

    def answer(self, status, body=None, headers=()):
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        data = b""
        if body is not None:
            data = json.dumps(body).encode()
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        q = dict(urllib.parse.parse_qsl(u.query))
        if u.path == "/adfs/.well-known/openid-configuration":
            return self.answer(200, discovery(ADFS, "/oauth2/authorize", "/oauth2/token", "/discovery/keys"))
        if u.path == "/site/.well-known/openid-configuration":
            return self.answer(200, discovery(SITE, "/authorize", "/token", "/keys"))
        if u.path == "/site/keys":
            return self.answer(200, {"keys": [{"kty": "RSA", "kid": "e2e", "alg": "RS256", "use": "sig",
                                               "n": b64(int_bytes(N)), "e": b64(int_bytes(E))}]})
        if u.path == "/site/authorize":
            if (q.get("client_id") != CLIENT_ID or q.get("redirect_uri") != REDIRECT_URI or q.get("response_type") != "code"
                    or "openid" not in q.get("scope", "").split() or not q.get("state") or not q.get("nonce")):
                return self.answer(400, {"error": "invalid_request"})
            code = secrets.token_urlsafe(24)
            with LOCK:
                CODES[code] = q["nonce"]
            to = REDIRECT_URI + "?" + urllib.parse.urlencode({"code": code, "state": q["state"]})
            return self.answer(302, headers=[("Location", to)])
        return self.answer(404, {"error": "not_found"})

    def do_POST(self):
        u = urllib.parse.urlsplit(self.path)
        if u.path != "/site/token":
            return self.answer(404, {"error": "not_found"})
        form = dict(urllib.parse.parse_qsl(self.rfile.read(int(self.headers.get("Content-Length", "0"))).decode()))
        client, secret = form.get("client_id"), form.get("client_secret")
        auth = self.headers.get("Authorization", "")
        if auth.startswith("Basic "):
            client, _, secret = base64.b64decode(auth[6:]).decode().partition(":")
            client, secret = urllib.parse.unquote_plus(client), urllib.parse.unquote_plus(secret)
        with LOCK:
            nonce = CODES.pop(form.get("code", ""), None)  # a code is good once
        if client != CLIENT_ID or secret != CLIENT_SECRET:
            return self.answer(401, {"error": "invalid_client"})
        if nonce is None or form.get("grant_type") != "authorization_code" or form.get("redirect_uri") != REDIRECT_URI:
            return self.answer(400, {"error": "invalid_grant"})
        now = int(time.time())
        claims = {"iss": SITE, "aud": CLIENT_ID, "sub": SUBJECT, "email": EMAIL, "email_verified": True,
                  "nonce": nonce, "iat": now, "exp": now + 300}
        signed = b64(json.dumps({"alg": "RS256", "typ": "JWT", "kid": "e2e"}).encode()) + "." + b64(json.dumps(claims).encode())
        token = signed + "." + b64(sign(signed.encode()))
        return self.answer(200, {"access_token": secrets.token_urlsafe(16), "token_type": "Bearer", "expires_in": 300,
                                 "id_token": token})


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
