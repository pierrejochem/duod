# duod messaging

## One-liner
GitOps blue/green for Docker Compose, switched by Traefik.

## Taglines
- Two slots. One daemon.
- Git is the truth. Traefik flips the traffic.
- Argo-style rollouts for a plain Docker host.
- Zero-downtime releases without Kubernetes.

## Elevator pitch (30 s)
duod is a small Go daemon that brings the Argo CD and Argo Rollouts blueGreen workflow to a single Docker host. Push to Git, duod starts the new color with Docker Compose, probes it through Traefik, then switches traffic. If the health check fails, traffic never moves.

## Boilerplate (press / README / directory listings)
duod is an open-source (Apache-2.0) GitOps operator for Docker Compose. It keeps two colors of every service, rolls out new versions in parallel per service, verifies health before switching, and uses Traefik (or Apache) to move traffic with zero downtime. Docs: https://pierrejochem.github.io/duod/ · Source: https://github.com/pierrejochem/duod

## Audience and pain
| Audience | Pain | Message |
|---|---|---|
| Homelab / small-team operators | Kubernetes is too heavy for one VPS | Same rollout safety, one binary |
| Teams on Compose + Traefik | `docker compose up` causes blips | Blue/green with automatic rollback |
| Argo CD users with edge boxes | Two toolchains | Same concepts, mapped 1:1 |

## Proof points
- Three services rolled out concurrently, 2,239+ requests per service, zero failures in the demo.
- 5-minute demo: `make demo-up`.
- Packages: deb, rpm, tarballs for amd64 and arm64.

## Launch copy

**Hacker News / Reddit title**
Show HN: duod – GitOps blue/green for Docker Compose, switched by Traefik

**Post body**
I wanted Argo Rollouts-style blue/green on a single Docker host without running Kubernetes. duod watches a Git repo, brings up the new color with Compose, health-checks it behind Traefik, and only then flips traffic. Each service rolls out independently and in parallel. Go, Apache-2.0, deb/rpm packages, 5-minute demo included.
https://github.com/pierrejochem/duod

**X / Mastodon / Bluesky (≤280)**
Blue/green releases for Docker Compose, driven by Git. duod starts the new color, probes it via Traefik, then switches. Failed health check = traffic never moves. Open source, Apache-2.0. https://github.com/pierrejochem/duod

**LinkedIn**
Not every workload needs Kubernetes, but every release deserves a safe rollout. duod brings the Argo CD + Argo Rollouts workflow to a plain Docker host: Git as source of truth, Compose for the two colors, Traefik for the switch. Independent services, parallel rollouts, automatic rollback. Try the 5-minute demo: https://pierrejochem.github.io/duod/

## Hashtags
#gitops #docker #dockercompose #traefik #bluegreen #devops #selfhosted #golang

## GitHub repo settings
- Description: GitOps blue/green for Docker Compose, switched by Traefik
- Website: https://pierrejochem.github.io/duod/
- Social preview: upload `brand/social/github-social-preview.png`
