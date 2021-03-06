scionApp
    .factory('userService', ["$http", "$q", function ($http, $q) {
    return {
        // Load the user's data
        userPageData: function () {
            return $http.get('/api/userPageData').then(function (response) {
                console.log(response);
                return response.data;
            });
        },
        // Create SCIONLab AS
        generateSCIONLabAS: function () {
            return $http.post('/api/as/generateAS').then(function (response) {
                console.log(response);
                return response.data;
            });
        },
        // Configure SCIONLab AS
        configureSCIONLabAS: function (user, asInfo) {
            let request = {
                asID: asInfo.ASID,
                userEmail: user.Email,
                isVPN: asInfo.IsVPN,
                ip: asInfo.IP,
                serverIA: asInfo.AP,
                label: asInfo.Label,
                type: asInfo.Type == "2" ? 2 : 1,
                port: asInfo.Port,
            };
            console.log(request);
            return $http.post('/api/as/configureAS', request).then(function (response) {
                console.log(response);
                return response.data;
            });
        },
        // Remove SCIONLab AS
        removeSCIONLabAS: function (asID) {
            return $http.post('/api/as/removeAS/' + asID).then(function (response) {
                console.log(response);
                return response.data;
            });
        }
    };
}]);
