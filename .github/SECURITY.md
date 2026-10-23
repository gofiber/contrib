# Security Policy

1. [Supported Versions](#versions)
2. [Reporting security problems to Fiber](#reporting)
3. [Security Points of Contact](#contact)
4. [Incident Response Process](#process)
5. [Secure accounts with access](#accounts)
6. [Critical Updates And Security Notices](#notices)

<a name="versions"></a>

## Supported Versions

`gofiber/contrib` is a multi-module repository. Each middleware and service
implementation under [`v3/`](https://github.com/gofiber/contrib/tree/main/v3)
is its own Go module that is versioned and released independently, using tags
such as `v3/websocket/v1.2.0`, so there is no single version range that applies
to the whole repository.

Security fixes are published as a new release of the affected module, so we
recommend always running the latest release of every module you depend on. The
latest releases are listed on the
[releases page](https://github.com/gofiber/contrib/releases).

If you believe a vulnerability affects any release of a module, please
[report it](#reporting). You do not need to check first whether that release is
still supported; just include the module name and version in your report.

<a name="reporting"></a>

## Reporting security problems to Fiber

**DO NOT CREATE AN ISSUE** to report a security problem. Instead, please
send us an e-mail at `rene@gofiber.io` or join our discord server via
[this invite link](https://gofiber.io/discord) and send a private message
to any of the maintainers.

<a name="contact"></a>

## Security Points of Contact

For security-related matters, please contact any of the
[@maintainers](https://github.com/orgs/gofiber/teams/maintainers).

The maintainers are the only persons with administrative access to Fiber's source code
and respond to security incident reports as fast as possible, within one business day
at the latest.

<a name="process"></a>

## Incident Response Process

In case an incident is discovered or reported, we will follow the following
process to contain, respond and remediate:

### 1. Containment

The first step is to find out the root cause, nature and scope of the incident.

- Is the incident still ongoing? If yes, prioritize stopping it.
- Is the incident outside our influence? If yes, prioritize containment.
- Find out who knows about the incident and who is affected.
- Find out what data was potentially exposed.

### 2. Response

After the initial assessment and containment to our best abilities, we will
document all actions taken in a response plan.

We will create a comment in the official `#announcements` channel to inform users about
the incident and what actions we took to contain it.

### 3. Remediation

Once the incident is confirmed to be resolved, we will summarize the lessons
learned from the incident and create a list of actions we will take to prevent
it from happening again.

<a name="accounts"></a>

## Secure accounts with access

The [Fiber Organization](https://github.com/gofiber) requires 2FA authorization
for all of its members.

<a name="notices"></a>

## Critical Updates And Security Notices

We learn about critical software updates and security threats from these sources

1. GitHub Security Alerts
2. GitHub: [https://status.github.com/](https://status.github.com/) & [@githubstatus](https://twitter.com/githubstatus)
