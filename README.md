# dy.fi ip updater

## What does it do?

Checks every 3 seconds if ip to outside requests is changed. If ip changes, a request is sent to dy.fi.
Also, the request to dy.fi is made every 5 days.

## Build

`go build`

## Running

dyUsername=USERNAME dyPassword=PASSWORD ./dyfi-ip-update