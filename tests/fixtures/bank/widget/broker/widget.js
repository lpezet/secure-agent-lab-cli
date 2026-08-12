// Fixture broker provider, minimal. Nothing here is exposed to the lab.
const audit = require("../audit");

module.exports = function register(app) {
  app.get("/widget/token", (req, res) => res.json({ fixture: true }));
};
