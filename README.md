# maildotapp

This `maildotapp` Go package provides an interface for programatically reading email message files stored by the MacOS Mail.app.

Specfically, it takes care of:

-   querying the Mail.app sqlite database to return the path on disk to individual Mail.app email
-   stripping proprietary Apple-specific header/footer data in Mail.app email files, so that emails can be parsed by "net/mail" or similar packages

See the [./example](./example) for more details and usage information.
