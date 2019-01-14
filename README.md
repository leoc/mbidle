# MBidle

`MBidle` is a ruby daemon that uses IMAP idle to invoke
[mbsync/isync](http://isync.sourceforge.net/) when ever a new message
was saved a remote IMAP folder.

## Usage

To install the package use rubygems:

    gem install mbidle
    
And run `mbidle` in the commandline. I used my `xinitrc` file to run
`mbidle &` in the background, but currently I run the background
process via awesomewm config file.

# TODO

- [X] Implement executable
- [X] Publish ruby gem for easy installation
- [ ] Add configurable `after_sync` / `on_local_change` hooks
