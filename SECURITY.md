# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/spencerlys/tokenrank-agent/security/advisories/new)
("Security" tab → "Report a vulnerability"). Do not open a public issue.

Include what you found, how to reproduce it, and the version
(`tokenrank-agent version`). We aim to acknowledge reports within a few days
and will keep you updated until a fix is released.

Never include real API keys or tokens in a report. If you think a key was
exposed, rotate it with your provider first.

## What we especially want to hear about

- any way the endpoint key could leave the device other than in a request to
  the user's configured endpoint;
- any way a batch could make the agent send to a host other than the
  configured base URL;
- bypasses of batch signature, binding or expiry checks;
- secrets written to disk, logs or the upload payload;
- the audit sidecar being reachable from another machine.

## Supported versions

Only the latest release receives security fixes.
