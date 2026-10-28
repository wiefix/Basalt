// An app without a Dockerfile, built by Railpack in make test-install.
require("http")
  .createServer((req, res) => res.end("built by railpack, listening on " + process.env.PORT))
  .listen(process.env.PORT);
