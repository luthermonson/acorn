args: build: image: string

acorns: {
	sub1: {
		build: "./subdir"
	}
	sub2: {
		build: {
			context: "./subdir2"
			acornfile: "./subdir2/test.cue"
			buildArgs: {
				filename: "buildfile"
				image: args.build.image
			}
		}
	}
}