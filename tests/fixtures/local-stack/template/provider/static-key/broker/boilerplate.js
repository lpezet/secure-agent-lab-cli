// Fixture skeleton. Deliberately NOT the placeholder the real stack uses:
// sal must read the token out of this manifest's own `name`, so a fixture
// spelling it differently is what proves nothing is hardcoded.
const audit = require("../audit");

module.exports = {
  "/boilerplate/cred": async (url, send) => {
    const token = tryReadFile(process.env.BOILERPLATE_TOKEN_PATH);
    audit({ provider: "boilerplate", event: "cred_issued" });
    send(200, { token });
  },
};
