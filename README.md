# websent

This is a tool for quick and easy presentations in the browser.

Write your slides in markdown.  
Serve them over http as an in-browser presentation.  
Controlled via CLI.  
Doesn't need any JavaScript.

First, install the tool with:
```
$ go get -u github.com/jktr/websent
```

You can then run the tool like this.  
An example presentation can be found in the /example directory.
```
$ websent presentation.md
Listening on http://[::1]:8080
1/7>
```

Note:
- You will probably want to provide your own stylesheet.
- There is no TLS support. Use a reverse proxy if you need it.

[Suckess' sent](https://tools.suckless.org/sent)
previously filled this tool's niche for me, but there
are some some issues with wayland, low-bandwidth
streaming, and missing support for fancier typesetting.
While not in the least bit suckless, using a
browser as the rendering platform addresses
these issues somewhat.

This is an enhanced port of a tech demo originally
developed by [thelegy](https://github.com/thelegy), which he
built for a talk at [C3PB](https://c3pb.de/blog/lightning-talks-0x0c-keepassxc-mf70-cnc-webseiten-ohne-js.html).

