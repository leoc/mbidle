# MBidle

`MBidle` is a ruby daemon that uses IMAP idle to invoke
[mbsync/isync](http://isync.sourceforge.net/) when ever a new message
was saved a remote IMAP folder.

# TODO

- [ ] Implement executable
- [ ] Publish ruby gem for easy installation
- [ ] Add configurable `after_sync` / `on_local_change` hooks
