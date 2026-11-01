#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Minimal S3 client of lab E19 (runs inside a lab VM, standard library only).

Reads a dbbackup backend file (type s3) and signs requests with SigV4:

  s3tool.py BACKEND.json get KEY OUTFILE
  s3tool.py BACKEND.json put KEY INFILE
  s3tool.py BACKEND.json delete KEY
  s3tool.py BACKEND.json list [PREFIX]

KEY is the full object key including the configured prefix. It exists only to
tamper with and remove stored objects in the lab; it prints no credential.
"""
import datetime
import hashlib
import hmac
import http.client
import json
import re
import ssl
import sys
import tempfile
import urllib.parse


def sign(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()


def request(cfg, method, key, body=b"", query=""):
    s3 = cfg["s3"]
    u = urllib.parse.urlparse(s3["endpoint"])
    host = u.netloc
    path = "/%s/%s" % (s3["bucket"], urllib.parse.quote(key, safe="/-_.~")) if key else "/%s" % s3["bucket"]
    now = datetime.datetime.now(datetime.timezone.utc)
    amz, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    ph = hashlib.sha256(body).hexdigest()
    headers = {"host": host, "x-amz-content-sha256": ph, "x-amz-date": amz}
    signed = ";".join(sorted(headers))
    canon = "\n".join([method, path, query, "".join("%s:%s\n" % (k, headers[k]) for k in sorted(headers)), signed, ph])
    scope = "%s/%s/s3/aws4_request" % (day, s3["region"])
    sts = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canon.encode()).hexdigest()])
    k = sign(sign(sign(sign(("AWS4" + s3["secret_key"]).encode(), day), s3["region"]), "s3"), "aws4_request")
    sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s" % (s3["access_key"], scope, signed, sig)
    with tempfile.NamedTemporaryFile("w", suffix=".pem") as ca:
        ca.write(s3["ca_pem"])
        ca.flush()
        ctx = ssl.create_default_context(cafile=ca.name)
        ctx.verify_flags &= ~ssl.VERIFY_X509_STRICT  # the lab CA has no keyUsage extension (Go accepts it)
    conn = http.client.HTTPSConnection(u.hostname, u.port, context=ctx)
    conn.request(method, path + ("?" + query if query else ""), body=body, headers=headers)
    r = conn.getresponse()
    return r.status, r.read()


def main():
    cfg = json.load(open(sys.argv[1]))
    op = sys.argv[2]
    if op == "get":
        st, data = request(cfg, "GET", sys.argv[3])
        open(sys.argv[4], "wb").write(data)
        print("GET", st, len(data), "bytes sha256", hashlib.sha256(data).hexdigest())
    elif op == "put":
        data = open(sys.argv[4], "rb").read()
        st, _ = request(cfg, "PUT", sys.argv[3], data)
        print("PUT", st, len(data), "bytes sha256", hashlib.sha256(data).hexdigest())
    elif op == "delete":
        st, _ = request(cfg, "DELETE", sys.argv[3])
        print("DELETE", st)
    elif op == "list":
        prefix = sys.argv[3] if len(sys.argv) > 3 else ""
        st, data = request(cfg, "GET", "", query="list-type=2&prefix=" + urllib.parse.quote(prefix, safe=""))
        print("LIST", st)
        for key in re.findall(r"<Key>([^<]*)</Key>", data.decode()):
            print(key)
    else:
        sys.exit("unknown operation")


main()
