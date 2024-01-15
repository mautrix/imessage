# BlueBubbles iMessage Bridge Interface

## Prerequisties

1. Ensure you have a Mac System with the Messages App working
1. Install [BlueBubbles Server](https://bluebubbles.app/downloads/server/) on it.
   1. Accept all the defaults while installing the server
   1. Ignore Private API during the initial install, it can be enabled later.
1. (Optional) Enable [BlueBubbles Private API](https://docs.bluebubbles.app/private-api/installation)
   1. Note: This requires disabling System Integrity Protection (SIP), which is not a wise thing to do on a Mac you use out in the world on a regular basis.
   1. Not enabling Private API makes the following features unavailable:
      1. "Tap Backs": aka Emoji Reactions
      1. Please let us know if there's more you find that doesn't work...
1. Familiarize yourself with the [BlueBubbles API](https://documenter.getpostman.com/view/765844/UV5RnfwM#4e5fd735-bd88-41c1-bc8f-96394b91f5e6)

## Development

<!-- TODO: This experience could be greatly improved -->

1. Download the most recent `bbctl` from the most recent [GitHub Actions](https://github.com/beeper/bridge-manager/actions) build
1. Move it somewhere on your system that is in your `$PATH`, and `chmod +x bbctl`
1. Run `bbctl login` to login to your Beeper Account
1. Install [`pre-commit`](https://pre-commit.com/#install)

### Running Locally

1. Clone this repository: `git clone git@github.com:mautrix/imessage.git`
1. `cd` into `imessage`
1. Setup `pre-commit` hooks: `pre-commit install`
1. Switch to the `bluebubbles` branch: `git checkout bluebubbles`
   1. This is temporary while we're developing this feature.
   1. You'll know you're in the right spot if you can see this README in your local code.
1. Run the following command to launch the bridge in development mode:

```bash
bbctl run --local-dev --param 'bluebubbles_url=<YOUR BLUEBUBBLES URL>' --param 'bluebubbles_password=<YOUR BLUEBUBBLES PASSWORD>' --param 'imessage_platform=bluebubbles' sh-imessage
```

_NOTE_: Double check that the `config.yaml` that was automatically generated has the correct values set for `bluebubbles_url` and `bluebubbles_password`, as sometimes `bbctl run` doesn't copy the `param` properly.

### Contributing

1. Find an open issue or bug
1. Create and switch a fork of this repository and branch
1. Hack away
1. Submit a PR

#### Join us the community chat

1. In the Beeper Desktop App, click the Settings Gear (Cog) at the top
1. Add a server: `maunium.net`
1. Find the iMessage Room: `#imessage:maunium.net`
1. Say hello!
