/* Resolves argv[1] with the C library's getaddrinfo and prints each IPv4
 * address; built static against musl for the fence's resolver test. */
#include <arpa/inet.h>
#include <netdb.h>
#include <stdio.h>
#include <string.h>

int main(int argc, char **argv) {
	struct addrinfo hints, *res, *p;
	char buf[INET_ADDRSTRLEN];
	if (argc != 2) {
		fprintf(stderr, "usage: musl-resolve NAME\n");
		return 2;
	}
	memset(&hints, 0, sizeof hints);
	hints.ai_family = AF_INET;
	hints.ai_socktype = SOCK_STREAM;
	int err = getaddrinfo(argv[1], NULL, &hints, &res);
	if (err != 0) {
		fprintf(stderr, "musl-resolve: %s: %s\n", argv[1], gai_strerror(err));
		return 1;
	}
	for (p = res; p != NULL; p = p->ai_next) {
		struct sockaddr_in *in = (struct sockaddr_in *)p->ai_addr;
		printf("%s\n", inet_ntop(AF_INET, &in->sin_addr, buf, sizeof buf));
	}
	freeaddrinfo(res);
	return 0;
}
