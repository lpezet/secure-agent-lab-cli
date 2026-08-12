#!/bin/sh
# Fixture lab_setup fragment, sourced by the deployment.
#
# Permitted to exist because it runs on the untrusted side already: it can do
# nothing the lab could not already do for itself.
git config --global credential.helper '!f() { curl -s "$ACME_CREDENTIAL_URL"; }; f'
