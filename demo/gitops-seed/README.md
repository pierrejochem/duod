# duod demo - GitOps repository

One folder per independent service. duod watches each folder separately:
a commit to `apps/api` only rolls out `api`.

Release = change the `image:` tag in `apps/<service>/compose.yaml`, commit, push.
