window.pollerJob = {};
window.poller = function(target, url, delay) {
	// Don't allow duplicate targets, so it's safe to call poller multiple times.
	if (window.pollerJob[target] === "active") {
		return;
	}
	window.pollerJob[target] = "active";

	var old = '';
	var p = function() {
		// Target is gone, so we're done.
		if ($(target).length === 0) {
			delete window.pollerJob[target];
			return
		}

		// Make the request.
		$.ajax({
			url: url,
			type: 'GET',
			success: function(data) {
				// A doctype tag indicates we're getting a full HTML response, not a fragment.
				if (data.substring(0, 50).toLowerCase().indexOf("doctype") !== -1) {
					return;
				}
				// We only update the target if there is a change.
				if (data !== old) {
					$(target).html(data);
					old = data;
				}
			},
			complete: function() {
				setTimeout(p, delay);
			}
		});
	};
	p();
};

