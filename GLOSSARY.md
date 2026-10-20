# GCP Observability

Observability investigations use delegated Google access to inspect logs, metrics,
traces, errors and profiles.

## Language

**Grant family**:
One authorization and the sequence of refresh tokens issued from it. Replay or
revocation ends access for the whole family.
_Avoid_: User session

**Source profile**:
An immutable recorded sample of a program's resource usage. A comparison uses a
current source profile and a base source profile; their difference is not another
source profile.
_Avoid_: Cached diff profile
