# Extproc experiment for envoy

All of the code is almost entirely generated using claude. This is an attempt to understand how extproc can be used alongside envoy to add arbitrary logic as a http filter. 

Wasm filter is one option but it has a lot of limitations and it is very difficult to write correctly, but it grants us better performance. Whereas extproc forces envoy to make an external call to a grpc service
defined by us, to do the same thing. Since it is an independent service, none of the wasm limitations apply here, but you pay with performance.

The main reason I tried this was redis. I was mostly able to do all the authentication logic in web-assembly through C++, but I hit a roadblock when I tried to use redis as a cache for all the external calls
I was making from the wasm binary. Since there is no way to open raw sockets from the wasm vm, the only way was to use the plugins provided by envoy and there was none for making tcp level connections. So after
attempting to write a second network level wasm filter to act as a http proxy for redis, I realized even that was not enough, as if i ever had to make multiple redis commands per http request ( to the network filter ),
I would still be stuck, since there was no way to change the "downstream" buffer once the "upstream" has already responded in a network layer filter.
