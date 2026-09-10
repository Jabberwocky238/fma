#!/usr/bin/env python3
"""Read-only SMTP connectivity checks; sends no mail or authentication credentials."""
import concurrent.futures
import socket
import time


def probe(host, port):
    try:
        address = socket.getaddrinfo(host, port, socket.AF_INET, socket.SOCK_STREAM)[0][4][0]
    except OSError as error:
        return f'{host}:{port} DNS error: {error}'
    start = time.monotonic()
    try:
        with socket.create_connection((address, port), timeout=8) as connection:
            connection.settimeout(8)
            response = connection.recv(512).decode(errors='replace').splitlines()[0]
    except OSError as error:
        response = f'{type(error).__name__}: {error}'
    return f'{host} {address}:{port} {time.monotonic()-start:.1f}s {response}'


if __name__ == '__main__':
    targets = [('gmail-smtp-in.l.google.com',25), ('alt1.gmail-smtp-in.l.google.com',25),
               ('mx3.qq.com',25), ('smtp.gmail.com',587)]
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        for result in pool.map(lambda target: probe(*target), targets):
            print(result)
