# Desktop GUI

The desktop application lives in its own repository:

**https://github.com/masoodhumza60/EasyCLIProxyAPIPlus**

It is a fork of `router-for-me/EasyCLIProxyAPI`, and it installs the core
published by *this* repository. Its updater is pointed here, so a release
tagged in this repository is what the application installs.

## Why it is separate

The two projects change for different reasons and at different rates. Keeping
the application out of this repository means an upstream change to the desktop
shell can be pulled in without touching the proxy, and a change to the proxy
cannot break the application's build.

It also keeps this repository to one thing: the server.

## Installing a core from here

The application downloads the core on first run. A release must exist in this
repository for it to find one - see the release notes for the current version.