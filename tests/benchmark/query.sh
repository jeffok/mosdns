#!/bin/sh
dig @127.0.0.1 "$1" +time=1 +tries=1 +short >/dev/null 2>&1 || true
