// Fixture broker provider. Not functional: it exists so the installer has a
// real file to place at /app/providers/acme.js, and something with stable
// contents for drift detection to compare against.
//
// The require path is the one that matters. It resolves only from the image
// the manifest's min_stack names, which is why installing into a deployment
// pinned below it fails at runtime rather than at install.
const audit = require("../audit");

module.exports = function register(app) {
  // Never whitelisted: mints the reusable credential.
  app.get("/acme/token", (req, res) => res.json({ fixture: true }));

  // Whitelisted: what the lab is allowed to ask for.
  app.get("/acme/credential", (req, res) => res.json({ fixture: true }));
  app.get("/acme/identity", (req, res) => res.json({ fixture: true }));
};
